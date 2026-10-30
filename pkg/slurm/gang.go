package slurm

// Gang scheduling.
//
// The pods of one group are submitted as ONE `sbatch --nodes=N` co-scheduled
// SLURM job instead of one sbatch each (one job, N ranks on N nodes), which
// co-allocates the entire gang atomically. Each pod is rendered as /create
// renders it (its own dir and job.sh); the gang job runs every member's job.sh
// on its own node, or the site launcher the head pod names (launcher.go).
//
// Groups arrive whole through /createPodGroup (podgroup.go), only when
// Config.GangSchedulingEnabled is set; /create keeps one job per pod.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"al.essio.dev/pkg/shellescape"
)

// gangCoordinationPort is the coordination port exported to every rank as
// MASTER_PORT / the Ray GCS port. Set to Ray's native GCS port 6379 so KubeRay's
// injected RAY_PORT/RAY_ADDRESS env (which hardcode 6379) stay CONSISTENT with the
// port the head actually binds -- otherwise a Ray client that discovers the
// cluster via the RAY_ADDRESS env var (rather than an explicit --address) would
// dial the wrong port. torch/MPI don't care about the specific value, so 6379 is
// safe for all frameworks. Fixed for now; a future change can make it per-gang /
// annotation-driven.
const gangCoordinationPort = 6379

// preExecAnnotation is shell the job script runs before the workload.
const preExecAnnotation = "slurm-job.vk.io/pre-exec"

// produceGangSLURMScript renders ONE co-scheduled sbatch script for the whole
// gang into headMember.WorkDir/job.slurm and returns its path:
//   - #SBATCH --nodes=N --ntasks-per-node=1, --job-name=<gang>, output to the
//     head member's dir (reusing buildSbatchFlags for flavor/annotation flags,
//     with the default --mem=1 suppressed),
//   - a head-address block (scontrol show hostnames + srun hostname --ip-address),
//   - generic coordination env exported to every rank (MASTER_ADDR/MASTER_PORT/
//     WORLD_SIZE/RANK/RAY_ADDRESS),
//   - one per-rank `srun --overlap --nodes=1 --ntasks=1 -w <node>` that runs the
//     member's OWN job.sh (already written by produceSLURMScript into that
//     member's dir), routing -o/-e into that member's own dir. Running each
//     member's job.sh means each rank produces the SAME per-pod artifacts a
//     single-pod job would (run-<ctn>.out and run-<ctn>.status) in its own dir.
//   - a readiness barrier after the head before the workers start,
//   - teardown that reaps the daemons and exits with the head/driver rc.
//
// Per-pod status behaviour (what actually happens, no overstatement):
// each gang member DIVERGES on its OWN terminal exit: its run-<ctn>.status is
// read by the gang-only branch in Status.go, so a failed rank shows Failed on its
// own pod while the gang and the other ranks keep running (no scancel). What
// remains INHERENTLY SHARED is whole-allocation SLURM state: an operator scancel,
// a node failure, or the walltime limit terminate the one job, so all member pods
// observe those together via the shared squeue JID. That shared-infrastructure
// ceiling is expected and by design; only per-rank workload exit codes are
// isolated (via each member's own run-<ctn>.status file).
func produceGangSLURMScript(
	ctx context.Context,
	config SlurmConfig,
	entry *GangEntry,
	ordered []*BufferedMember,
) (string, error) {
	if len(ordered) == 0 {
		return "", fmt.Errorf("gang %s: no members to render", entry.Name)
	}
	head := ordered[0]
	headPath := head.WorkDir
	n := len(ordered)

	if err := os.MkdirAll(headPath, os.ModePerm); err != nil {
		log.G(ctx).Error(err)
		return "", err
	}

	// --- #SBATCH header --------------------------------------------------------
	// Reuse the head member's flavor/annotation flags, sized for the largest
	// member (gangNodeShape), with default --mem suppressed. --nodes /
	// --ntasks-per-node are gang-specific and prepended explicitly. Deduplication in buildSbatchFlags
	// runs on the flavor/annotation set; our fixed --nodes/--ntasks-per-node are
	// added after and are not user-overridable here.
	sizePod, sizeLimits, sizeDefaultCPU, sizeDefaultRAM := gangNodeShape(ctx, ordered)
	flags := buildSbatchFlags(ctx, config, sizePod, head.Pod.ObjectMeta,
		sizeLimits, sizeDefaultCPU, sizeDefaultRAM, head.Flavor, true /*suppressDefaultMem*/)
	gpusPerNode := gpusPerNodeFromFlags(flags)
	// GPUFlag gpus counts the GPUs of the whole job, not of a node: the pod's count
	// is asked for on every node, as /estimate does.
	if config.GPUFlag == GPUFlagGPUs {
		if gpus := detectGPUResources(ctx, sizePod.Spec.Containers); gpus > 0 {
			if i := slices.Index(flags, "--gpus="+strconv.FormatInt(gpus, 10)); i >= 0 {
				flags[i] = "--gpus=" + strconv.FormatInt(gpus*int64(n), 10)
				gpusPerNode = int(gpus)
			}
		}
	}

	// --- launcher selection (injected-launcher path) --------------------------
	// interlink.eu/launcher names a function of the site file (`<name>-launcher`),
	// which starts the whole gang. Checked here: a launcher that cannot run fails
	// the submission instead of falling back to another way of starting the job.
	site, hasSite, serr := loadSite(ctx, config)
	if serr != nil {
		return "", fmt.Errorf("gang %s: %w", entry.Name, serr)
	}
	launcherName := selectedLauncherName(head.Pod.ObjectMeta)
	if launcherName != "" {
		if !hasSite {
			return "", fmt.Errorf("gang %s: launcher %q requested but there is no site file at %s", entry.Name, launcherName, siteScriptPath(config))
		}
		if err := site.checkLauncher(launcherName); err != nil {
			return "", fmt.Errorf("gang %s: %w", entry.Name, err)
		}
	}
	// #SBATCH --ntasks-per-node: launchers the site marks with interlink-tasks-per-gpu
	// (mpirun takes its slots from the allocation) get one task per GPU; everything
	// else one task per node.
	tasksPerNode := 1
	if launcherName != "" && site.TasksPerGPU[launcherName] && gpusPerNode > 0 {
		tasksPerNode = gpusPerNode
	}

	var hdr strings.Builder
	hdr.WriteString("#!" + config.BashPath + "\n")
	hdr.WriteString("#SBATCH --job-name=" + entry.Name + "\n")
	hdr.WriteString("#SBATCH --nodes=" + strconv.Itoa(n) + "\n")
	hdr.WriteString("#SBATCH --ntasks-per-node=" + strconv.Itoa(tasksPerNode) + "\n")
	// Batch-body stdout/stderr (driver output) lands in the head member's dir so
	// the plugin harvests it as the head pod's job.out (per-pod-dir log model).
	hdr.WriteString("#SBATCH --output=" + headPath + "/job.out\n")
	for _, f := range flags {
		if strings.TrimSpace(f) == "" {
			continue
		}
		// Skip a stray --nodes/--ntasks-per-node from flavor/annotations: the gang
		// size is authoritative and already emitted above.
		key := slurmFlagKey(f)
		if key == "--nodes" || key == "--ntasks-per-node" {
			continue
		}
		hdr.WriteString("#SBATCH " + f + "\n")
	}

	// --- body ------------------------------------------------------------------
	var body strings.Builder
	body.WriteString("set -uo pipefail\n\n")
	body.WriteString("# interLink gang-scheduling job (group " + entry.Name + ")\n")
	body.WriteString("# One sbatch --nodes=" + strconv.Itoa(n) + " co-allocates the whole gang; each rank runs on one node.\n\n")

	// Site setup (e.g. `module load singularity`) at the BATCH level, mirroring the
	// single-pod path in produceSLURMScript. The single-pod script runs the
	// CommandPrefix in the batch shell, so its PATH changes are visible to job.sh.
	// The gang used to run the CommandPrefix inside each per-rank `srun bash -c`, a
	// FRESH sub-shell where the `module` shell-function is often NOT defined (the
	// interLink sbatch is submitted via a non-login `ssh <host> sbatch`), so
	// `module load singularity` silently no-oped, `singularity` was off PATH, and
	// job.sh's bare `singularity` exited 127. Running it ONCE here executes
	// `module load` in the batch shell (where `module` IS defined) and the resulting
	// PATH propagates to every rank via `srun --export=ALL`. It also writes the
	// compute-node file exactly once (from the head/batch node) instead of the old
	// racing per-rank writes.
	if strings.TrimSpace(config.Commandprefix) != "" {
		body.WriteString("# --- site CommandPrefix (batch level; PATH propagates to ranks via --export=ALL) ---\n")
		body.WriteString(config.Commandprefix + "\n\n")
	}

	// The site file's top level (modules, exports) runs in the batch shell of every
	// gang; its <name>-launcher functions become callable.
	if hasSite {
		sitePath, werr := writeSiteFile(headPath, site)
		if werr != nil {
			return "", fmt.Errorf("gang %s: %w", entry.Name, werr)
		}
		body.WriteString("# --- site file (" + site.Path + ") ---\n")
		body.WriteString("source " + shellescape.Quote(sitePath) + "\n\n")
	}

	// HPC-tuning hook (BOTH modes): the head pod's slurm-job.vk.io/mpi-env
	// annotation ("K=V;K=V") is materialized as `export SINGULARITYENV_<K>=<V>`
	// in the batch shell BEFORE the launch section. Singularity's SINGULARITYENV_
	// prefix mechanism MERGES these into every rank's container env (it does NOT
	// clobber the per-rank --env-file that carries MASTER_ADDR/RANK/WORLD_SIZE, so
	// torch can inject NCCL_* / UCX_* env here without breaking rendezvous). The
	// exports also stay visible to the host launcher (UCX/PMIx server side) for the
	// MPI path. Empty/malformed pairs (no '=', empty key) are skipped.
	if exports := gangMPIEnvExports(head.Pod.ObjectMeta); len(exports) > 0 {
		body.WriteString("# --- HPC tuning env (slurm-job.vk.io/mpi-env -> SINGULARITYENV_*; merged into every rank) ---\n")
		for _, e := range exports {
			body.WriteString(e + "\n")
		}
		body.WriteString("\n")
	}

	// Deterministic head-node discovery for the shadow/tunnel pods. The batch
	// script executes on nodes[0], which IS rank 0's (the head's) node, so
	// `hostname -f` here is the head node. Publish it under a well-known,
	// gang-keyed path: the VK's wstunnel template exposes only name / namespace /
	// labels / annotations (NO pod UID, verified against interLink 0.6.1), so a
	// shadow pod cannot resolve the head member's per-UID dir - but it CAN fetch
	// this file by the group's name. A recreated gang generation simply
	// overwrites the file (latest wins). Best-effort:
	// discovery must never kill the workload.
	headDiscovery := config.DataRootFolder + gangHeadsDirName + "/" + head.Namespace + "-" + entry.Name
	body.WriteString("# --- publish head node for shadow/tunnel discovery (batch node == head node) ---\n")
	body.WriteString("mkdir -p " + shellescape.Quote(config.DataRootFolder+gangHeadsDirName) +
		" && hostname -f > " + shellescape.Quote(headDiscovery) + " || true\n\n")

	port := gangCoordinationPort

	// Resolve the allocation's nodes and the head IP.
	body.WriteString(`mapfile -t nodes < <(scontrol show hostnames "$SLURM_JOB_NODELIST")` + "\n")
	body.WriteString(`head_node="${nodes[0]}"` + "\n")
	body.WriteString(`head_ip=$(srun --nodes=1 --ntasks=1 -w "$head_node" hostname --ip-address | awk '{print $1}')` + "\n")
	body.WriteString("export MASTER_ADDR=\"$head_ip\"\n")
	body.WriteString("export MASTER_PORT=" + strconv.Itoa(port) + "\n")
	body.WriteString("export WORLD_SIZE=" + strconv.Itoa(n) + "\n")
	body.WriteString("export RAY_ADDRESS=\"$head_ip:" + strconv.Itoa(port) + "\"\n")
	body.WriteString(`echo "gang " ` + shellescape.Quote(entry.Name) + ` " head_node=$head_node head_ip=$head_ip world_size=` + strconv.Itoa(n) + `"` + "\n")
	// Gang topology exported for injected launchers (launcher_main reads these).
	body.WriteString("export GANG_NNODES=" + strconv.Itoa(n) + "\n")
	body.WriteString("export GANG_NODES=\"${nodes[*]}\"\n")
	body.WriteString("export GANG_HEAD_NODE=\"$head_node\"\n")
	body.WriteString("export GANG_GPUS_PER_NODE=" + strconv.Itoa(gpusPerNode) + "\n\n")
	emitGangSocketDirs(&body, config.SocketDirRoot, ordered)

	// --- launch section --------------------------------------------------------
	//   - interlink.eu/launcher: the site's <name>-launcher starts every rank;
	//   - otherwise each pod runs its own command on its own node (one
	//     `srun --overlap --nodes=1 --ntasks=1` per member, head first).
	if launcherName != "" {
		// Probes check the pod's own command, which a site launcher replaces: drop
		// their metadata so the status reports the containers ready while they run.
		for _, m := range ordered {
			for _, rc := range m.RuntimeCommands {
				_ = os.Remove(fmt.Sprintf("%s/probe-metadata-%s.txt", m.WorkDir, rc.containerName))
			}
		}
		emitSiteLauncher(&body, ordered, launcherName, config.SocketDirRoot)
	} else {
		emitGangPerRankLaunch(&body, ordered, n, port, config.SocketDirRoot)
	}

	// --- write job.slurm -------------------------------------------------------
	scriptPath := headPath + "/job.slurm"
	full := hdr.String() + "\n" + body.String()
	if err := os.WriteFile(scriptPath, []byte(full), 0o600); err != nil {
		log.G(ctx).Error("Unable to write gang job.slurm ", scriptPath, ": ", err)
		return "", err
	}
	log.G(ctx).Infof("Rendered gang sbatch for '%s' (%d nodes) at %s", entry.Name, n, scriptPath)
	return scriptPath, nil
}

// gangNodeShape returns what the per-node flags are built from: the head, with
// the largest CPU, memory and GPU request of any member. Every rank gets one node
// of the same shape, so members that differ (several PodSets of a Workload) must
// each fit in it; sized from the head alone, a larger worker would not.
func gangNodeShape(ctx context.Context, ordered []*BufferedMember) (v1.Pod, ResourceLimits, bool, bool) {
	head := ordered[0]
	pod, limits := head.Pod, head.ResourceLimits
	defaultCPU, defaultRAM := head.IsDefaultCPU, head.IsDefaultRAM
	gpus := detectGPUResources(ctx, pod.Spec.Containers)
	for _, m := range ordered[1:] {
		if !m.IsDefaultCPU && (defaultCPU || m.ResourceLimits.CPU > limits.CPU) {
			limits.CPU, defaultCPU = m.ResourceLimits.CPU, false
		}
		if !m.IsDefaultRAM && (defaultRAM || m.ResourceLimits.Memory > limits.Memory) {
			limits.Memory, defaultRAM = m.ResourceLimits.Memory, false
		}
		if n := detectGPUResources(ctx, m.Pod.Spec.Containers); n > gpus {
			pod.Spec.Containers, gpus = m.Pod.Spec.Containers, n
		}
	}
	return pod, limits, defaultCPU, defaultRAM
}

// emitGangPerRankLaunch renders the DEFAULT per-rank launch section (Ray/torch):
// one `srun --overlap --nodes=1 --ntasks=1` per member, a readiness barrier
// before the workers, and DRIVER_RC = rank-0's rc as the whole-gang result. Used
// when the pods name no site launcher.
func emitGangPerRankLaunch(bodyPtr *strings.Builder, ordered []*BufferedMember, n, port int, socketRoot string) {
	body := bodyPtr
	// Track background PIDs so we can wait/teardown.
	body.WriteString("declare -a RANK_PIDS=()\n\n")

	// Emit one srun per rank. Rank 0 (head) launches first, then a readiness
	// barrier, then the remaining ranks. Each srun runs the member's OWN job.sh
	// (already written by produceSLURMScript into m.WorkDir), so the member's
	// containers run with all the usual per-pod bookkeeping (run-<ctn>.out /
	// run-<ctn>.status) landing in that member's dir -> isolated per-pod logs and
	// status. The gang coordination env is exported before job.sh runs.
	emitRank := func(m *BufferedMember) {
		node := "${nodes[" + strconv.Itoa(m.Rank) + "]}"
		out := m.WorkDir + "/job.out"
		errOut := m.WorkDir + "/job.err"
		memberScript := m.WorkDir + "/job.sh"
		// Record THIS rank's compute node into THIS member's own dir, then exec the
		// member's job.sh. The write runs inside the rank's srun (hostname -f on the
		// rank's node - in a --nodes=N gang the worker is on a DIFFERENT node than
		// the head), giving true per-member node discovery for the shadow/tunnel
		// pods. Best-effort (|| true): a failed write must not kill the rank.
		//
		// The site CommandPrefix (module loads etc.) is emitted ONCE at the batch
		// level above; its PATH changes reach this rank via `srun --export=ALL`, so
		// the member's job.sh finds its container runtime. We deliberately do NOT
		// re-run the CommandPrefix here: a per-rank `srun bash -c` is a fresh
		// sub-shell where the `module` shell-function is often undefined, so
		// `module load singularity` would no-op and job.sh's bare `singularity`
		// would exit 127 (the original bug). Coordination env is passed via srun
		// --export below (so $head_ip resolves in the batch shell).
		inner := "hostname -f > " + shellescape.Quote(m.WorkDir+"/compute-node") + " || true\n"
		// The head's pre-exec (the reverse tunnel's set-up) runs in the head's own
		// rank, on the node its shadow reaches, with the head's socket directory.
		// The workers' pre-exec is not run: their nodes have no tunnel end.
		if pre := m.Pod.Annotations[preExecAnnotation]; m.Rank == 0 && strings.TrimSpace(pre) != "" {
			if socketDir := socketDirFor(socketRoot, m.PodUID); socketDir != "" {
				inner += "export INTERLINK_SOCKET_DIR=" + shellescape.Quote(socketDir) + "\n"
			}
			inner += pre + "\n"
		}
		inner += "exec " + shellescape.Quote(memberScript)
		body.WriteString("# rank " + strconv.Itoa(m.Rank) + " (" + m.Role + ") pod " + m.PodUID + "\n")
		body.WriteString("srun --overlap --nodes=1 --ntasks=1 -w \"" + node + "\" \\\n")
		// Coordination env via srun --export: $head_ip resolves in the batch shell
		// (double-quoted here), so the value is baked into the srun command line and
		// reaches the task regardless of the site's default SLURM export policy.
		body.WriteString("     --export=ALL,RANK=" + strconv.Itoa(m.Rank) + ",WORLD_SIZE=" + strconv.Itoa(n) +
			",MASTER_ADDR=\"$head_ip\",MASTER_PORT=" + strconv.Itoa(port) +
			",RAY_ADDRESS=\"$head_ip:" + strconv.Itoa(port) + "\" \\\n")
		body.WriteString("     -o " + shellescape.Quote(out) + " -e " + shellescape.Quote(errOut) + " --open-mode=truncate \\\n")
		body.WriteString("  bash -c " + shellescape.Quote(inner) + " &\n")
		body.WriteString("RANK_PIDS+=($!)\n\n")
	}

	// Head first.
	emitRank(ordered[0])

	// Readiness barrier before workers: poll `ray status` against the head GCS.
	// Best-effort: if `ray` is not on PATH (a
	// non-Ray workload), the probe simply times out and we proceed anyway so
	// non-Ray gangs are not blocked.
	if n > 1 {
		body.WriteString("# readiness barrier: wait for the head coordinator before launching workers\n")
		body.WriteString("head_ready=0\n")
		body.WriteString("for i in $(seq 1 60); do\n")
		body.WriteString("  # Generic readiness: the head (rank 0) opens MASTER_PORT (Ray GCS / torch\n")
		body.WriteString("  # rendezvous). Probe the port from the head node ($head_ip/$MASTER_PORT\n")
		body.WriteString("  # expand in the batch shell). The loop proceeds anyway after the timeout so\n")
		body.WriteString("  # a workload that never opens the port is not blocked forever.\n")
		body.WriteString("  if srun --overlap --nodes=1 --ntasks=1 -w \"$head_node\" bash -c \"exec 3<>/dev/tcp/$head_ip/$MASTER_PORT\" >/dev/null 2>&1; then head_ready=1; break; fi\n")
		body.WriteString("  sleep 2\n")
		body.WriteString("done\n")
		body.WriteString("echo \"gang head_ready=$head_ready\"\n\n")

		// Then the workers.
		for _, m := range ordered[1:] {
			emitRank(m)
		}
	}

	// Wait for all ranks; capture the head/driver rc from rank 0 so the pod exit
	// status reflects the workload, not a blocking daemon.
	body.WriteString("# Wait for the head (rank 0 / driver) and capture its rc as the gang result.\n")
	body.WriteString("DRIVER_RC=0\n")
	body.WriteString("if [ \"${#RANK_PIDS[@]}\" -gt 0 ]; then\n")
	body.WriteString("  wait \"${RANK_PIDS[0]}\"; DRIVER_RC=$?\n")
	body.WriteString("fi\n")
	body.WriteString("# Reap remaining ranks so the allocation frees.\n")
	body.WriteString("for pid in \"${RANK_PIDS[@]:1}\"; do kill \"$pid\" 2>/dev/null || true; done\n")
	body.WriteString("wait 2>/dev/null || true\n")
	body.WriteString("echo \"DRIVER_RC=$DRIVER_RC\"\n")
	body.WriteString("exit \"$DRIVER_RC\"\n")
}

// emitSiteLauncher renders the launch section of a gang whose pods name a site
// launcher: `<name>-launcher` runs ONCE from the batch script (on the head's node)
// with the head pod's command as arguments and starts every rank itself. Its exit
// code ends every pod.
//
// Logs: rank i writes to $GANG_LOG_DIR/rank-<i>.out (srun --output=...rank-%n.out),
// which is pod i's log file; whatever the function prints itself goes to rank 0.
func emitSiteLauncher(body *strings.Builder, ordered []*BufferedMember, name, socketRoot string) {
	head := ordered[0]
	prefix, appArgs, _, ok := launcherWorkload(head.RuntimeCommands)
	if !ok {
		body.WriteString("echo 'gang launcher: the head pod has no workload container to launch' >&2\n")
		body.WriteString("exit 1\n")
		return
	}
	body.WriteString("# --- site launcher '" + name + "': " + name + launcherFuncSuffix + " starts every rank ---\n")
	// srun no longer propagates cpus-per-task to steps; forward it explicitly.
	body.WriteString(`export SRUN_CPUS_PER_TASK="${SLURM_CPUS_PER_TASK:-}"` + "\n")
	// GANG_CONTAINER = the container-exec prefix (runtime options + image) of the head
	// pod; launchers use it unquoted, e.g. `srun ... $GANG_CONTAINER "$@"`.
	body.WriteString("export GANG_CONTAINER=" + shellescape.Quote(prefix) + "\n")
	body.WriteString("hostname -f > " + shellescape.Quote(head.WorkDir+"/compute-node") + " || true\n")
	// The other members' pods learn their node too, or their shadows never can (and a
	// virtual kubelet that holds a pod until its shadow serves never readies them): rank i
	// runs on the allocation's i-th node, as the site launchers start them (GANG_NODES order).
	for _, m := range ordered[1:] {
		body.WriteString(`srun --overlap -N1 -n1 -w "${nodes[` + strconv.Itoa(m.Rank) + `]}" hostname -f > ` +
			shellescape.Quote(m.WorkDir+"/compute-node") + " || true\n")
	}
	if socketDir := socketDirFor(socketRoot, head.PodUID); socketDir != "" {
		body.WriteString("export INTERLINK_SOCKET_DIR=" + shellescape.Quote(socketDir) + "\n")
	}

	logDir := head.WorkDir + "/ranks"
	body.WriteString("export GANG_LOG_DIR=" + shellescape.Quote(logDir) + "\n")
	body.WriteString(`mkdir -p "$GANG_LOG_DIR"` + "\n")
	for _, m := range ordered {
		_, _, ctnName, ok := launcherWorkload(m.RuntimeCommands)
		if !ok {
			continue
		}
		rankLog := logDir + "/rank-" + strconv.Itoa(m.Rank) + ".out"
		body.WriteString(": > " + shellescape.Quote(rankLog) + "\n")
		body.WriteString("ln -sfn " + shellescape.Quote(rankLog) + " " + shellescape.Quote(m.WorkDir+"/run-"+ctnName+".out") + "\n")
	}

	// A subshell: a launcher may end in exec, which must not replace the batch shell
	// before the status files below are written.
	body.WriteString("( " + name + launcherFuncSuffix + " " + shellescapeArgs(appArgs) + " ) >>" +
		shellescape.Quote(logDir+"/rank-0.out") + " 2>&1\n")
	body.WriteString("LAUNCHER_RC=$?\n")
	body.WriteString("echo \"gang launcher '" + name + "' exited rc=$LAUNCHER_RC\"\n")
	for _, m := range ordered {
		if _, _, ctnName, ok := launcherWorkload(m.RuntimeCommands); ok {
			body.WriteString("echo \"$LAUNCHER_RC\" > " + shellescape.Quote(m.WorkDir+"/run-"+ctnName+".status") + " || true\n")
		}
	}
	body.WriteString("exit \"$LAUNCHER_RC\"\n")
}

// emitGangSocketDirs creates every member's private socket directory (as a single
// pod's job.sh does): the members' container invocations bind them, and a launcher
// path never runs the members' job.sh. The batch shell removes them on exit.
func emitGangSocketDirs(body *strings.Builder, root string, ordered []*BufferedMember) {
	if root == "" {
		return
	}
	dirs := make([]string, 0, len(ordered))
	body.WriteString("(umask 077 && mkdir -p " + shellescape.Quote(root) + ")\n")
	for _, m := range ordered {
		d := shellescape.Quote(socketDirFor(root, m.PodUID))
		dirs = append(dirs, d)
		body.WriteString("rm -rf " + d + "; (umask 077 && mkdir " + d + " && chmod 0700 " + d + ") || { echo \"interLink: cannot create a private socket directory\" >&2; exit 1; }\n")
	}
	body.WriteString("trap " + shellescape.Quote("rm -rf "+strings.Join(dirs, " ")) + " EXIT\n\n")
}

// gangMPIEnvExports parses the head pod's slurm-job.vk.io/mpi-env annotation
// ("K=V;K=V") into a slice of `export SINGULARITYENV_<K>=<shellescaped V>` lines.
// Empty or malformed pairs (no '=', empty key) are skipped. Returns nil when the
// annotation is absent or yields no valid pairs.
// isShellEnvName reports whether s is a valid POSIX shell/env variable name:
// [A-Za-z_][A-Za-z0-9_]*. Used to reject unsafe mpi-env keys before they are
// interpolated (unquoted) into an `export` statement.
func isShellEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			// always allowed
		case i > 0 && r >= '0' && r <= '9':
			// digits allowed after the first char
		default:
			return false
		}
	}
	return true
}

func gangMPIEnvExports(metadata metav1.ObjectMeta) []string {
	raw := strings.TrimSpace(metadata.Annotations["slurm-job.vk.io/mpi-env"])
	if raw == "" {
		return nil
	}
	var out []string
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		// The KEY is interpolated unquoted into `export SINGULARITYENV_<K>=...`, so
		// only accept valid shell env-var names ([A-Za-z_][A-Za-z0-9_]*). This drops
		// a hostile key like `X;rm -rf /` before it can break out of the export line
		// (the value is already shellescaped; this hardens the key side too).
		if !isShellEnvName(k) {
			continue
		}
		out = append(out, "export SINGULARITYENV_"+k+"="+shellescape.Quote(v))
	}
	return out
}

// slurmJobGone classifies one squeue probe of a submitted gang's JID and
// reports whether the job is DEFINITIVELY gone from SLURM, so that a group sent
// again under the same name may be submitted afresh. The decision is
// deliberately asymmetric: resubmitting over a live job is the worse failure
// mode (two allocations writing the same member dirs), so anything ambiguous
// reports NOT gone.
//   - stderr "Invalid job id" -> gone: the controller no longer knows the job
//     (it ran and aged out, exactly what a dead gang looks like after MinJobAge).
//   - any OTHER stderr (e.g. a transient SSH failure through the site's squeue
//     shim) -> NOT gone: indeterminate, never risk a double submit.
//   - clean run, empty output -> gone (with --states=all a live job always prints).
//   - terminal StateCompact code -> gone; live or unrecognized codes -> NOT gone.
func slurmJobGone(stdout, stderr string, exitCode int) bool {
	errS := strings.TrimSpace(stderr)
	if errS != "" {
		// rc-independent: squeue exits 1 alongside this message.
		return strings.Contains(errS, "Invalid job id")
	}
	// go-execute v1 returns err=nil for ANY nonzero exit (the failure lives only
	// in ExitCode), so a signal-killed ssh shim surfaces here as nonzero rc with
	// EMPTY stdout and stderr - maximally ambiguous. Without this gate the empty-
	// output branch below would declare a live gang gone and double-submit
	// (found by adversarial review, reproduced against go-execute v0.6.0). On a
	// nonzero exit, distrust stdout too (could be partial pipe output): only the
	// explicit Invalid-job-id marker above may declare gone.
	if exitCode != 0 {
		return false
	}
	out := strings.TrimSpace(stdout)
	if out == "" {
		return true
	}
	switch strings.Fields(out)[0] {
	// Unambiguously terminal squeue StateCompact codes. PR (preempted) is
	// deliberately absent: a preempt-requeued job returns to PD.
	case "CD", "CA", "F", "TO", "NF", "BF", "DL", slurmStateOOM, "SE":
		return true
	}
	return false
}

// slurmStateOOM is squeue's compact state of a job killed for running out of memory.
const slurmStateOOM = "OOM"

// squeueNoHeader drops squeue's header line.
const squeueNoHeader = "--noheader"

// gangJobIsGone asks SLURM whether the submitted gang JID still exists, using
// the same squeue invocation shape as Status.go. A failure to even run squeue
// reports NOT gone (see slurmJobGone's asymmetry rationale).
func gangJobIsGone(ctx context.Context, config SlurmConfig, jid string) bool {
	shell := exec.ExecTask{
		Command: config.Squeuepath,
		// "-j", jid as two arguments: a shim that forwards arguments faithfully would
		// otherwise pass squeue a blank job ID.
		Args:  []string{squeueNoHeader, "-a", "--states=all", "-O", "StateCompact", "-j", jid},
		Shell: true,
	}
	execReturn, err := shell.Execute()
	if err != nil {
		log.G(ctx).Warning("Gang staleness probe: could not run squeue for JID "+jid+": ", err, " (treating as alive)")
		return false
	}
	return slurmJobGone(execReturn.Stdout, execReturn.Stderr, execReturn.ExitCode)
}

// gangHeadsDirName is the well-known directory (under DataRootFolder) where a
// gang's batch script publishes the head node's hostname, keyed
// <namespace>-<group name>. Shadow/tunnel pods resolve the head through it
// (they know the group's name but not any pod UID).
const gangHeadsDirName = ".gang-heads"

// gangMarkerFile is the per-member marker file that flags a pod dir as belonging
// to a co-scheduled gang. Its presence (checked by LoadJIDs and Status) is what
// enables the gang-only per-rank status divergence; single-pod jobs never write
// it, so their status behaviour is unchanged.
const gangMarkerFile = "gang.marker"

// readMemberContainerExitCode reads a gang member's OWN per-container exit code
// from run-<ctn>.status (or init-<ctn>.status) WITHOUT ever writing anything.
// It returns (exitCode, found, err):
//   - found=false, err=nil  : no status file yet -> this rank is still running.
//   - found=true            : the rank's container has terminated with exitCode.
//
// It deliberately does NOT reuse getExitCode: that helper, on a missing file,
// synthesises a status file from the shared squeue exit code, which for a still-
// Running gang would wrongly record a terminal state. This read-only variant is
// what lets a single failed rank surface as Failed on its own pod while the
// shared SLURM job (and the other ranks) keep running.
func readMemberContainerExitCode(path, ctName string) (int32, bool, error) {
	for _, p := range []string{path + "/run-" + ctName + ".status", path + "/init-" + ctName + ".status"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, false, err
		}
		code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return 0, false, fmt.Errorf("parse %s: %w", p, err)
		}
		return int32(code), true, nil
	}
	return 0, false, nil
}

// stampJID mirrors handleJidAndPodUid's on-disk bookkeeping for one member so
// LoadJIDs can rehydrate every gang member on plugin restart. It writes
// JobID.jid / PodUID.uid / PodNamespace.ns (and WorkDir.path when the job dir is
// elsewhere) into the member's OWN dir, writes the gang.marker (so this member is
// recognised as a gang member across restarts), and registers the JID in the
// shared JIDs map with Gang=true. The caller MUST hold h.GangMu.
func stampJID(ctx context.Context, jids *map[string]*JidStruct, uid, ns, jid, dir, workDir string) error {
	if err := os.WriteFile(dir+"/JobID.jid", []byte(jid), 0o600); err != nil {
		log.G(ctx).Error("Can't write JobID.jid for ", uid, ": ", err)
		return err
	}
	if err := os.WriteFile(dir+"/PodNamespace.ns", []byte(ns), 0o600); err != nil {
		log.G(ctx).Error("Can't write PodNamespace.ns for ", uid, ": ", err)
		return err
	}
	if err := os.WriteFile(dir+"/PodUID.uid", []byte(uid), 0o600); err != nil {
		log.G(ctx).Error("Can't write PodUID.uid for ", uid, ": ", err)
		return err
	}
	if err := os.WriteFile(dir+"/"+gangMarkerFile, []byte(jid), 0o600); err != nil {
		log.G(ctx).Error("Can't write gang.marker for ", uid, ": ", err)
		return err
	}
	entry := &JidStruct{PodUID: uid, PodNamespace: ns, JID: jid, Gang: true}
	if workDir != dir {
		if err := os.WriteFile(dir+"/WorkDir.path", []byte(workDir), 0o600); err != nil {
			log.G(ctx).Error("Can't write WorkDir.path for ", uid, ": ", err)
			return err
		}
		entry.WorkDir = workDir
	}
	(*jids)[uid] = entry
	return nil
}

// errGangSubmitted marks a failure after the gang's sbatch ran: its job may run.
var errGangSubmitted = errors.New("sbatch ran")

// submitGang submits ONE sbatch for the members of entry, ordered by rank, and
// back-fills every member's JID. A failure after sbatch ran wraps
// errGangSubmitted. The caller MUST hold h.GangMu.
func (h *SidecarHandler) submitGang(ctx context.Context, entry *GangEntry, ordered []*BufferedMember) (string, error) {
	scriptPath, err := produceGangSLURMScript(ctx, h.Config, entry, ordered)
	if err != nil {
		return "", err
	}

	out, err := SLURMBatchSubmit(h.Ctx, h.Config, scriptPath)
	if err != nil {
		log.G(ctx).Error("Gang '"+entry.Name+"': sbatch failed: ", err)
		return "", err
	}
	log.G(ctx).Info(out)

	jid, err := parseJIDFromSubmit(out)
	if err != nil {
		return "", fmt.Errorf("gang %s: %w: %w", entry.Name, errGangSubmitted, err)
	}
	entry.JID = jid

	// Back-fill every member: register the shared JID in JIDs and persist the
	// per-pod bookkeeping files so LoadJIDs rehydrates all N on restart. If a
	// member cannot be stamped, none keeps the JID: the entry alone holds the job,
	// and the delete of its last pod cancels it (removeGangMemberOnDelete).
	for i, m := range ordered {
		if err := stampJID(ctx, h.JIDs, m.PodUID, m.Namespace, jid, m.FilesPath, m.WorkDir); err != nil {
			for _, stamped := range ordered[:i] {
				removeJID(stamped.PodUID, h.JIDs)
			}
			return "", fmt.Errorf("gang %s: %w as job %s: %w", entry.Name, errGangSubmitted, jid, err)
		}
	}
	entry.Submitted = true
	log.G(ctx).Infof("Gang '%s': submitted as SLURM job %s (%d nodes)", entry.Name, jid, len(ordered))

	return jid, nil
}

// removeGangMemberOnDelete drops a deleted pod's UID from its gang entry and
// removes the entry once empty. An entry whose job ID no member holds (sbatch
// ran, then the members could not be stamped) cancels that job when it empties.
// Safe to call for non-gang pods (no-op if the UID is not tracked). Takes
// h.GangMu.
func (h *SidecarHandler) removeGangMemberOnDelete(ctx context.Context, uid string) {
	h.GangMu.Lock()
	defer h.GangMu.Unlock()
	if h.GangTable == nil {
		return
	}
	for name, entry := range h.GangTable {
		if _, ok := entry.Members[uid]; ok {
			delete(entry.Members, uid)
			log.G(ctx).Infof("Gang '%s': removed member %s on delete (%d remaining)", name, uid, len(entry.Members))
			if len(entry.Members) == 0 && !entry.Submitted && entry.JID != "" {
				if err := scancelJob(ctx, h.Config, entry.JID); err != nil {
					log.G(ctx).Errorf("Gang '%s': could not cancel job %s: %v", name, entry.JID, err)
				}
			}
		}
		if len(entry.Members) == 0 {
			delete(h.GangTable, name)
			log.G(ctx).Infof("Gang '%s': entry dropped (empty)", name)
		}
	}
}

// parseJIDFromSubmit extracts the numeric SLURM job ID from sbatch output.
// Mirrors the regex in handleJidAndPodUid.
func parseJIDFromSubmit(output string) (string, error) {
	m := jidSubmitRe.FindStringSubmatch(output)
	if len(m) < 2 {
		return "", fmt.Errorf("could not parse SLURM job ID from sbatch output %q", output)
	}
	return m[1], nil
}

// gangMemberFromCreate packages the data Create.go already rendered for one pod
// into a BufferedMember. The caller sets its Rank and Role.
func gangMemberFromCreate(
	pod v1.Pod,
	filesPath string,
	workDir string,
	runtimeCommands []ContainerCommand,
	resourceLimits ResourceLimits,
	isDefaultCPU bool,
	isDefaultRAM bool,
	flavor *FlavorResolution,
) *BufferedMember {
	return &BufferedMember{
		PodUID:          string(pod.UID),
		Namespace:       pod.Namespace,
		FilesPath:       filesPath,
		WorkDir:         workDir,
		Pod:             pod,
		RuntimeCommands: runtimeCommands,
		ResourceLimits:  resourceLimits,
		IsDefaultCPU:    isDefaultCPU,
		IsDefaultRAM:    isDefaultRAM,
		Flavor:          flavor,
	}
}
