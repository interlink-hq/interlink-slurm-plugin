package slurm

// Site launchers.
//
// How a distributed job starts differs from one HPC site to the next: modules to
// load, srun or mpirun, PMIx flavour, fabric libraries to bind into the container,
// NCCL/UCX settings. None of that belongs in the plugin, so each site provides ONE
// bash file (SlurmConfig.SiteScriptPath, typically a mounted ConfigMap):
//
//   - its top level runs at the start of every gang job (modules, exports);
//   - it defines one function per framework, named `<name>-launcher`
//     (torchrun-launcher, mpi-launcher, ray-launcher, ...). A gang whose pods carry
//     `interlink.eu/launcher: <name>` is started by calling that function ONCE from
//     the batch script, with the head pod's command as arguments. The function
//     starts every rank itself (srun, mpirun) and its return code ends every pod.
//
// The plugin keeps what is the same everywhere: grouping the pods, one sbatch for
// the gang, the node list, the head address, per-pod status and logs. The contract
// a launcher function can rely on is documented in
// docs/docs/tutorial-admins/04-site-launchers.md.
//
// When a launcher is requested, any problem with the site file (missing, `bash -n`
// failure, function not defined) fails the gang submission instead of falling back
// to another way of starting it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/containerd/containerd/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"al.essio.dev/pkg/shellescape"
)

// defaultSiteScriptPath is where the site file is mounted when the config names none.
const defaultSiteScriptPath = "/etc/interlink/site/site.sh"

// siteFileName is the copy of the site file written next to job.slurm (on the jobs
// directory, so the HPC sees it at the same path) and sourced by the batch script.
const siteFileName = "site.sh"

// launcherFuncSuffix turns a launcher name into the site function that runs it.
const launcherFuncSuffix = "-launcher"

var (
	// launcherNameRe restricts launcher names to a token that is also a valid part
	// of a bash function name.
	launcherNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// siteFuncRe matches the definition of a `<name>-launcher` function.
	siteFuncRe = regexp.MustCompile(`(?m)^[ \t]*(?:function[ \t]+)?([A-Za-z0-9][A-Za-z0-9_.-]*)` + regexp.QuoteMeta(launcherFuncSuffix) + `[ \t]*(?:\([ \t]*\))?[ \t]*\{?[ \t]*$`)
	// tasksPerGPURe matches "# interlink-tasks-per-gpu: <name> [<name>...]": the
	// launchers that want one Slurm task per GPU (mpirun reads its slots from the
	// allocation); the others get one task per node.
	tasksPerGPURe = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*interlink-tasks-per-gpu:[ \t]*(.+?)[ \t]*$`)
	// gresGPURe extracts N from --gres=gpu:N or --gres=gpu:<type>:N.
	gresGPURe = regexp.MustCompile(`^--gres=gpu(?::[A-Za-z0-9_.-]+)*:(\d+)$`)
	// gpusPerNodeFlagRe extracts N from --gpus-per-node=N or --gpus-per-node=<type>:N.
	gpusPerNodeFlagRe = regexp.MustCompile(`^--gpus-per-node=(?:[A-Za-z0-9_.-]+:)?(\d+)$`)
)

// siteDef is a loaded and checked site file.
type siteDef struct {
	Path        string
	Body        string
	Launchers   map[string]bool // launcher names with a `<name>-launcher` function
	TasksPerGPU map[string]bool // launchers that get one Slurm task per GPU
}

func siteScriptPath(config SlurmConfig) string {
	if p := strings.TrimSpace(config.SiteScriptPath); p != "" {
		return p
	}
	return defaultSiteScriptPath
}

// selectedLauncherName returns the launcher a gang pod asks for, "" for none.
func selectedLauncherName(metadata metav1.ObjectMeta) string {
	return strings.TrimSpace(metadata.Annotations[LauncherAnnotation])
}

// loadSite reads and checks the site file. A missing file is not an error by
// itself (ok=false): it only is when a pod asks for a launcher.
func loadSite(ctx context.Context, config SlurmConfig) (*siteDef, bool, error) {
	path := siteScriptPath(config)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cannot read site file %s: %w", path, err)
	}
	if err := bashSyntaxCheck(ctx, path); err != nil {
		return nil, false, fmt.Errorf("site file %s: bash -n failed: %w", path, err)
	}
	body := string(raw)
	site := &siteDef{Path: path, Body: body, Launchers: map[string]bool{}, TasksPerGPU: map[string]bool{}}
	for _, m := range siteFuncRe.FindAllStringSubmatch(body, -1) {
		site.Launchers[m[1]] = true
	}
	for _, m := range tasksPerGPURe.FindAllStringSubmatch(body, -1) {
		for _, name := range strings.Fields(m[1]) {
			site.TasksPerGPU[name] = true
		}
	}
	return site, true, nil
}

// checkLauncher fails unless the site file defines the launcher a gang asks for.
func (s *siteDef) checkLauncher(name string) error {
	if !launcherNameRe.MatchString(name) {
		return fmt.Errorf("invalid launcher name %q (allowed pattern %s)", name, launcherNameRe.String())
	}
	if !s.Launchers[name] {
		return fmt.Errorf("site file %s does not define %s%s()", s.Path, name, launcherFuncSuffix)
	}
	return nil
}

// bashSyntaxCheck runs `bash -n <path>`. If no bash is present in the plugin
// container the check is skipped with a warning; the HPC shell still parses the file.
func bashSyntaxCheck(ctx context.Context, path string) error {
	var bin string
	for _, c := range []string{"bash", "/bin/bash", "/usr/bin/bash"} {
		if p, err := exec.LookPath(c); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		log.G(ctx).Warningf("site file syntax check skipped: no bash found in plugin container; relying on HPC-side shebang for %s", path)
		return nil
	}
	out, err := exec.CommandContext(ctx, bin, "-n", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeSiteFile copies the site file next to job.slurm, so a ConfigMap update never
// changes a job that is already queued. Returns the absolute path.
func writeSiteFile(headPath string, site *siteDef) (string, error) {
	p := headPath + "/" + siteFileName
	if err := os.WriteFile(p, []byte(site.Body), 0o644); err != nil { //nolint:gosec // read by the job on the HPC side
		return "", fmt.Errorf("write %s: %w", siteFileName, err)
	}
	return p, nil
}

// gpusPerNodeFromFlags extracts the GPUs-per-node count the allocation grants, from
// the resolved sbatch flags: --gpus-per-node wins, else --gres=gpu[:type]:N.
// Returns 0 when no GPU flag is present.
func gpusPerNodeFromFlags(flags []string) int {
	for _, f := range flags {
		if m := gpusPerNodeFlagRe.FindStringSubmatch(strings.TrimSpace(f)); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	for _, f := range flags {
		if m := gresGPURe.FindStringSubmatch(strings.TrimSpace(f)); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// launcherWorkload extracts, from a member's rendered container commands, the
// container-exec PREFIX (runtimeCommand joined, e.g. `singularity exec <opts> <SIF>`,
// exported as GANG_CONTAINER) and the APP argv (containerCommand + containerArgs,
// passed to the launcher function). It uses the FIRST non-init container.
func launcherWorkload(rcs []ContainerCommand) (prefix string, appArgs []string, ctnName string, ok bool) {
	for i := range rcs {
		if rcs[i].isInitContainer {
			continue
		}
		rc := &rcs[i]
		app := append([]string{}, rc.containerCommand...)
		app = append(app, rc.containerArgs...)
		return strings.Join(rc.runtimeCommand, " "), app, rc.containerName, true
	}
	return "", nil, "", false
}

// shellescapeArgs joins argv into a single shell-safe, space-separated string.
func shellescapeArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellescape.Quote(a)
	}
	return strings.Join(parts, " ")
}
