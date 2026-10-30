package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
)

// --- test helpers -----------------------------------------------------------

// newGangPod builds a minimal pod with one container.
func newGangPod(uid string, annotations map[string]string) v1.Pod {
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-" + uid,
			Namespace:   testNamespace,
			UID:         types.UID(uid),
			Annotations: annotations,
		},
		Spec: v1.PodSpec{Containers: []v1.Container{{Name: testContainer, Image: busyboxImage}}},
	}
}

// writeStubScript writes an executable shell script and returns its path.
func writeStubScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // a fake executable
		t.Fatalf("write stub %s: %v", name, err)
	}
	return p
}

// newHandlerWithStubs returns a SidecarHandler whose sbatch echoes a fixed JID
// and whose scancel appends to a counter file. Returns the handler, the
// DataRootFolder, and the scancel counter file path.
func newHandlerWithStubs(t *testing.T, jid string) (*SidecarHandler, string, string) {
	t.Helper()
	bin := t.TempDir()
	dataRoot := t.TempDir() + string(os.PathSeparator)

	sbatch := writeStubScript(t, bin, "sbatch", "echo \"Submitted batch job "+jid+"\"")
	scancelCount := filepath.Join(bin, "scancel.count")
	scancel := writeStubScript(t, bin, "scancel", "echo \"$@\" >> "+scancelCount)

	jids := make(map[string]*JidStruct)
	h := &SidecarHandler{
		Config: SlurmConfig{
			BashPath:              testBashPath,
			Sbatchpath:            sbatch,
			Scancelpath:           scancel,
			DataRootFolder:        dataRoot,
			ContainerRuntime:      singularityRuntime,
			SingularityPath:       singularityRuntime,
			GangSchedulingEnabled: true,
		},
		JIDs:      &jids,
		Ctx:       context.Background(),
		GangTable: make(map[string]*GangEntry),
	}
	return h, dataRoot, scancelCount
}

// scancelCalls returns how many times the stub scancel was invoked.
func scancelCalls(t *testing.T, counterFile string) int {
	t.Helper()
	b, err := os.ReadFile(counterFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read scancel counter: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

// memberDir returns the per-pod dir for a uid under dataRoot.
func memberDir(dataRoot, uid string) string {
	return dataRoot + "default-" + uid
}

// mkMember builds a member for pod uid with its dir under dataRoot.
func mkMember(t *testing.T, dataRoot, uid string, annotations map[string]string) *BufferedMember {
	t.Helper()
	dir := memberDir(dataRoot, uid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return gangMemberFromCreate(newGangPod(uid, annotations), dir, dir, nil, ResourceLimits{}, true, true, nil)
}

// gangOf builds the entry of a gang whose members are ranked in the order
// given, as /createPodGroup ranks them: rank 0 is the head.
func gangOf(name string, members ...*BufferedMember) (*GangEntry, []*BufferedMember) {
	entry := &GangEntry{Name: name, Size: len(members), Members: map[string]*BufferedMember{}}
	for i, m := range members {
		m.Rank, m.Role = i, GangRoleWorker
		if i == 0 {
			m.Role = GangRoleHead
		}
		entry.Members[m.PodUID] = m
	}
	return entry, members
}

// gangScript renders the gang job of entry and returns it.
func gangScript(t *testing.T, config SlurmConfig, entry *GangEntry, ordered []*BufferedMember) string {
	t.Helper()
	path, err := produceGangSLURMScript(context.Background(), config, entry, ordered)
	if err != nil {
		t.Fatalf("produceGangSLURMScript: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read gang script: %v", err)
	}
	return string(b)
}

// submitMembers submits the pods as one gang, the way createPodGroup does once
// the pods are rendered: each member dir holds a minimal job.sh.
func submitMembers(t *testing.T, h *SidecarHandler, name string, pods ...v1.Pod) string {
	t.Helper()
	members := make([]*BufferedMember, 0, len(pods))
	for _, pod := range pods {
		filesPath := h.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID)
		if err := os.MkdirAll(filesPath, 0o755); err != nil {
			t.Fatalf("mkdir member dir: %v", err)
		}
		if err := os.WriteFile(filesPath+"/job.sh", []byte("#!/bin/bash\ntrue\n"), 0o755); err != nil { //nolint:gosec // a fake executable
			t.Fatalf("write member job.sh: %v", err)
		}
		members = append(members, gangMemberFromCreate(pod, filesPath, filesPath, nil, ResourceLimits{}, true, true, nil))
	}
	entry, ordered := gangOf(name, members...)
	h.GangMu.Lock()
	defer h.GangMu.Unlock()
	jid, err := h.submitGang(h.Ctx, entry, ordered)
	if err != nil {
		t.Fatalf("submitGang: %v", err)
	}
	h.GangTable[name] = entry
	return jid
}

// deletePod sends the pod to /delete (StopHandler). It takes no *testing.T so
// that it can run off the test goroutine.
func deletePod(h *SidecarHandler, pod v1.Pod) error {
	body, err := json.Marshal(pod)
	if err != nil {
		return err
	}
	rec := httptest.NewRecorder()
	h.StopHandler(rec, httptest.NewRequest(http.MethodPost, "/delete", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		return fmt.Errorf("delete %s: code %d, body %q", pod.UID, rec.Code, rec.Body.String())
	}
	return nil
}

// --- tests ------------------------------------------------------------------

// One submission back-fills every member: the shared JID in JIDs, flagged as a
// gang member, and the bookkeeping files LoadJIDs reads after a restart.
func TestSubmitGangBackfillsEveryMember(t *testing.T) {
	h, dataRoot, _ := newHandlerWithStubs(t, "999")
	entry, ordered := gangOf("g1", mkMember(t, dataRoot, "h", nil), mkMember(t, dataRoot, "w", nil))

	h.GangMu.Lock()
	jid, err := h.submitGang(h.Ctx, entry, ordered)
	h.GangMu.Unlock()
	if err != nil || jid != "999" {
		t.Fatalf("submitGang: jid=%q err=%v, want 999", jid, err)
	}
	if !entry.Submitted || entry.JID != "999" {
		t.Errorf("entry not marked submitted with job 999: %+v", entry)
	}
	for _, uid := range []string{"h", "w"} {
		e, ok := (*h.JIDs)[uid]
		if !ok || e.JID != "999" || !e.Gang {
			t.Errorf("member %s in JIDs = %+v, want gang job 999", uid, e)
		}
		for _, f := range []string{"JobID.jid", gangMarkerFile} {
			if b, err := os.ReadFile(memberDir(dataRoot, uid) + "/" + f); err != nil || string(b) != "999" {
				t.Errorf("member %s %s = %q, %v; want 999", uid, f, b, err)
			}
		}
	}
}

// cancel during execution: 2 members share a JID -> 2 deletes -> exactly ONE
// scancel; both dirs removed and the gang entry dropped.
func TestCancelDuringExecutionRefcountsScancel(t *testing.T) {
	h, dataRoot, scancelCount := newHandlerWithStubs(t, "555")
	submitMembers(t, h, "g1", newGangPod("h", nil), newGangPod("w", nil))

	deleteMember := func(uid string) {
		if err := deletePod(h, newGangPod(uid, nil)); err != nil {
			t.Fatal(err)
		}
	}

	// First delete: sibling still references the JID -> NO scancel yet.
	deleteMember("h")
	if got := scancelCalls(t, scancelCount); got != 0 {
		t.Errorf("after first delete scancel calls = %d, want 0 (sibling still live)", got)
	}
	// Second (last) delete: now scancel exactly once.
	deleteMember("w")
	if got := scancelCalls(t, scancelCount); got != 1 {
		t.Errorf("after last delete scancel calls = %d, want exactly 1", got)
	}

	for _, uid := range []string{"h", "w"} {
		if _, ok := (*h.JIDs)[uid]; ok {
			t.Errorf("JID entry for %s should be removed after delete", uid)
		}
		if _, err := os.Stat(memberDir(dataRoot, uid)); !os.IsNotExist(err) {
			t.Errorf("member dir for %s should be removed", uid)
		}
	}
	if _, ok := h.GangTable["g1"]; ok {
		t.Error("the gang entry outlived its members")
	}
}

// countJIDReferences underpins the refcount decision.
func TestCountJIDReferences(t *testing.T) {
	jids := map[string]*JidStruct{
		"a": {PodUID: "a", JID: "10"},
		"b": {PodUID: "b", JID: "10"},
		"c": {PodUID: "c", JID: "20"},
	}
	if n := countJIDReferences(&jids, "10"); n != 2 {
		t.Errorf("countJIDReferences(10) = %d, want 2", n)
	}
	if n := countJIDReferences(&jids, "20"); n != 1 {
		t.Errorf("countJIDReferences(20) = %d, want 1", n)
	}
	if n := countJIDReferences(&jids, ""); n != 0 {
		t.Errorf("countJIDReferences(\"\") = %d, want 0", n)
	}
}

// firstContainerState returns the first container's state from a PodStatus in
// the Status response, keyed by pod UID.
func firstContainerState(t *testing.T, resp []commonIL.PodStatus, uid string) v1.ContainerState {
	t.Helper()
	for _, ps := range resp {
		if ps.PodUID == uid {
			if len(ps.Containers) == 0 {
				t.Fatalf("pod %s has no container statuses", uid)
			}
			return ps.Containers[0].State
		}
	}
	t.Fatalf("pod %s not found in status response", uid)
	return v1.ContainerState{}
}

// TestGangRunningPerRankStatusIsolated drives StatusHandler END TO END and proves
// the per-rank behaviour: while the shared SLURM job is R (Running), a gang member
// whose own run-<ctn>.status shows a non-zero exit reports Terminated(Failed) on
// its OWN pod, while a sibling with no status file reports Running. It also
// asserts NO scancel is triggered by the rank failure.
func TestGangRunningPerRankStatusIsolated(t *testing.T) {
	h, dataRoot, scancelCount := newHandlerWithStubs(t, "777")

	// squeue stub: report the shared job as Running ("0 R") for both the --me and
	// the per-job -j queries StatusHandler makes.
	bin := filepath.Dir(h.Config.Sbatchpath)
	h.Config.Squeuepath = writeStubScript(t, bin, "squeue", `echo "0                 R"`)

	head := newGangPod("h", nil)
	worker := newGangPod("w", nil)
	submitMembers(t, h, "g1", head, worker)

	// The failing rank (worker) has written its own non-zero run-main.status; the
	// healthy rank (head) has NOT written any status file yet (still running).
	if err := os.WriteFile(memberDir(dataRoot, "w")+"/run-main.status", []byte("1\n"), 0o600); err != nil {
		t.Fatalf("write worker status: %v", err)
	}

	body, err := json.Marshal([]*v1.Pod{&head, &worker})
	if err != nil {
		t.Fatal(err)
	}

	// Reset the 10s status cache window so the handler recomputes.
	resetStatusCache()

	req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.StatusHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("StatusHandler status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp []commonIL.PodStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal status response: %v (body=%s)", err, rec.Body.String())
	}

	// Failed rank -> Terminated with exit code 1 on its own pod.
	wState := firstContainerState(t, resp, "w")
	if wState.Terminated == nil {
		t.Fatalf("worker (failed rank) should be Terminated, got %+v", wState)
	}
	if wState.Terminated.ExitCode != 1 {
		t.Errorf("worker exit code = %d, want 1", wState.Terminated.ExitCode)
	}
	// Healthy rank -> still Running (shared job is R and it has no status file).
	hState := firstContainerState(t, resp, "h")
	if hState.Running == nil {
		t.Errorf("head (healthy rank) should be Running, got %+v", hState)
	}

	// The rank failure must NOT have triggered any scancel.
	if scancelCalls(t, scancelCount) != 0 {
		t.Error("a single-rank failure must NOT trigger scancel")
	}
	// Both pods still share the same live JID.
	if (*h.JIDs)["h"].JID != (*h.JIDs)["w"].JID {
		t.Error("gang members must share one JID")
	}
}

// TestSinglePodRunningStatusUnaffected proves the gang divergence is gated: a
// NON-gang pod (Gang=false) with a stale run-<ctn>.status still reports Running
// under R, exactly as before the gang change (single-pod path byte-identical).
func TestSinglePodRunningStatusUnaffected(t *testing.T) {
	h, dataRoot, _ := newHandlerWithStubs(t, "42")
	bin := filepath.Dir(h.Config.Sbatchpath)
	h.Config.Squeuepath = writeStubScript(t, bin, "squeue", `echo "0                 R"`)

	// A plain single-pod job: register it in JIDs WITHOUT the gang flag and give
	// it a dir with a (would-be-misleading) non-zero status file present.
	uid := "solo"
	dir := memberDir(dataRoot, uid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dir+"/run-main.status", []byte("1\n"), 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}
	(*h.JIDs)[uid] = &JidStruct{PodUID: uid, PodNamespace: testNamespace, JID: "42"} // Gang=false

	pod := v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: testNamespace, UID: types.UID(uid)}}
	pod.Spec.Containers = []v1.Container{{Name: testContainer}}
	body, err := json.Marshal([]*v1.Pod{&pod})
	if err != nil {
		t.Fatal(err)
	}

	resetStatusCache()
	req := httptest.NewRequest(http.MethodPost, "/status", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.StatusHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("StatusHandler status = %d", rec.Code)
	}
	var resp []commonIL.PodStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	state := firstContainerState(t, resp, uid)
	if state.Running == nil {
		t.Errorf("single-pod job under R must report Running (gang divergence must NOT apply), got %+v", state)
	}
}

// restart recovery: after back-fill wrote JobID.jid/PodUID.uid/PodNamespace.ns
// into every member dir, LoadJIDs rehydrates all N with the shared JID.
func TestRestartRecoveryLoadsAllGangMembers(t *testing.T) {
	h, dataRoot, _ := newHandlerWithStubs(t, "888")
	submitMembers(t, h, "g1", newGangPod("h", nil), newGangPod("w", nil))

	// Fresh handler over the SAME DataRootFolder, empty JIDs (simulating restart).
	fresh := make(map[string]*JidStruct)
	h2 := &SidecarHandler{
		Config: SlurmConfig{DataRootFolder: dataRoot},
		JIDs:   &fresh,
		Ctx:    context.Background(),
	}
	if err := h2.LoadJIDs(); err != nil {
		t.Fatalf("LoadJIDs: %v", err)
	}
	for _, uid := range []string{"h", "w"} {
		e, ok := (*h2.JIDs)[uid]
		if !ok {
			t.Errorf("LoadJIDs did not rehydrate member %s", uid)
			continue
		}
		if e.JID != "888" || !e.Gang {
			t.Errorf("rehydrated %s = %+v, want gang job 888", uid, e)
		}
	}
}

// produceGangSLURMScript renders the co-scheduled shape with per-rank isolation.
func TestProduceGangSLURMScriptShape(t *testing.T) {
	dataRoot := t.TempDir() + string(os.PathSeparator)
	// A non-empty CommandPrefix mirrors a real deployment (e.g.
	// `module load singularity`); the gang script MUST emit it ONCE at the
	// batch level (see the regression assertions below).
	config := SlurmConfig{BashPath: testBashPath, Commandprefix: testModuleLoad}
	entry, ordered := gangOf("g1", mkMember(t, dataRoot, "h", nil), mkMember(t, dataRoot, "w", nil))
	s := gangScript(t, config, entry, ordered)

	for _, want := range []string{
		"#SBATCH --job-name=g1",
		"#SBATCH --nodes=2",
		"#SBATCH --ntasks-per-node=1",
		"scontrol show hostnames",
		"srun --overlap --nodes=1 --ntasks=1",
		"export MASTER_ADDR",
		"WORLD_SIZE=2",
		"DRIVER_RC",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("gang script missing %q\n---\n%s", want, s)
		}
	}
	// No default --mem=1 in the gang path (some sites reject an explicit --mem).
	if strings.Contains(s, "#SBATCH --mem=1\n") {
		t.Error("gang script must not emit the default --mem=1")
	}
	// Per-rank isolation: each member's own dir referenced for -o.
	if !strings.Contains(s, "default-h/job.out") || !strings.Contains(s, "default-w/job.out") {
		t.Error("each rank must route -o into its own member dir")
	}

	// REGRESSION: the site CommandPrefix (e.g. `module load singularity`) MUST run
	// ONCE at the BATCH level, not inside each per-rank `srun bash -c`. A per-rank
	// srun is a fresh sub-shell where the `module` shell-function is often undefined
	// (interLink submits via a non-login `ssh <host> sbatch`), so a per-rank
	// `module load singularity` no-ops, `singularity` is off PATH, and job.sh dies
	// with "singularity: command not found" (exit 127). Emitting it once at the
	// batch level runs `module load` where `module` is defined; the resulting PATH
	// then propagates to every rank via `srun --export=ALL`.
	if got := strings.Count(s, testModuleLoad); got != 1 {
		t.Errorf("CommandPrefix must run once at the batch level: got %d occurrences of 'module load singularity', want 1\n---\n%s", got, s)
	}
	// It must appear BEFORE the first per-rank srun (i.e. at the batch level). If it
	// came after, it would be inside a rank's bash -c (the 127 regression).
	if cp, sr := strings.Index(s, testModuleLoad), strings.Index(s, "srun --overlap"); cp < 0 || sr < 0 || cp > sr {
		t.Errorf("CommandPrefix must be emitted at the batch level (before the first per-rank srun): module@%d srun@%d\n---\n%s", cp, sr, s)
	}
	// The per-rank inner must be its own compute-node write followed by
	// `exec <job.sh>`, with NO CommandPrefix inlined (batch-level only).
	if !strings.Contains(s, "bash -c 'hostname -f > ") {
		t.Error("per-rank inner must start with its own compute-node write")
	}
	if c := strings.Count(s, "exec "); c < 2 {
		t.Errorf("each rank inner must exec its member job.sh (got %d exec occurrences)", c)
	}
	// REGRESSION: coordination env goes through `srun --export` (so $head_ip is
	// resolved in the batch shell), NOT re-exported inside the rank's single-quoted
	// `bash -c`, where $head_ip is empty and would clobber MASTER_ADDR/RAY_ADDRESS.
	for _, want := range []string{
		"--export=ALL,RANK=0",
		"--export=ALL,RANK=1",
		"MASTER_ADDR=\"$head_ip\"",
		"RAY_ADDRESS=\"$head_ip:6379\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("gang script missing coordination export %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "bash -c 'export RANK=") {
		t.Error("rank env must not be re-exported inside the single-quoted bash -c (empty $head_ip clobbers MASTER_ADDR/RAY_ADDRESS)")
	}
	// The readiness barrier must probe the head coordinator PORT (generic: Ray GCS /
	// torch rendezvous), not a bare `ray` binary that only exists inside the SIF.
	if !strings.Contains(s, "/dev/tcp/$head_ip/$MASTER_PORT") {
		t.Error("readiness barrier must probe the head MASTER_PORT, not a bare `ray` command")
	}
}

// Every rank gets one node of the same shape, so the per-node flags come from the
// largest CPU, memory and GPU request of any member, not from the head alone.
func TestGangNodeShapeTakesLargestMember(t *testing.T) {
	dataRoot := t.TempDir() + string(os.PathSeparator)
	config := SlurmConfig{BashPath: testBashPath}
	head := mkMember(t, dataRoot, "h", nil)
	worker := mkMember(t, dataRoot, "w", nil)
	worker.ResourceLimits = ResourceLimits{CPU: 8, Memory: 4 * 1024 * 1024 * 1024}
	worker.IsDefaultCPU, worker.IsDefaultRAM = false, false
	worker.Pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
	entry, ordered := gangOf("g1", head, worker)

	s := gangScript(t, config, entry, ordered)
	for _, want := range []string{"#SBATCH --cpus-per-task=8\n", "#SBATCH --mem=4096\n", "#SBATCH " + testGres4 + "\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("gang script missing %q, sized from the head alone\n---\n%s", want, s)
		}
	}
}

// Every GPUFlag asks for the pod's GPUs on every node, as /estimate does for the
// same shape: --gpus counts the whole job's, so it is the pod's count times the
// nodes.
func TestGangGPUFlagAsksEveryNode(t *testing.T) {
	for flag, want := range map[string]string{
		GPUFlagGres:        testGres4,
		GPUFlagGPUsPerNode: "--gpus-per-node=4",
		GPUFlagGPUs:        "--gpus=8",
	} {
		t.Run(flag, func(t *testing.T) {
			dataRoot := t.TempDir() + string(os.PathSeparator)
			config := SlurmConfig{BashPath: testBashPath, GPUFlag: flag}
			head, worker := mkMember(t, dataRoot, "h", nil), mkMember(t, dataRoot, "w", nil)
			for _, m := range []*BufferedMember{head, worker} {
				m.Pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
			}
			entry, ordered := gangOf("g1", head, worker)

			s := gangScript(t, config, entry, ordered)
			for _, line := range []string{"#SBATCH --nodes=2\n", "#SBATCH " + want + "\n", "export GANG_GPUS_PER_NODE=4\n"} {
				if !strings.Contains(s, line) {
					t.Errorf("gang script missing %q\n---\n%s", line, s)
				}
			}
			if est := strings.Join(estimateFlags(config, EstimateRequest{Nodes: 2, GPUsPerNode: 4}), " "); !strings.Contains(est, want) {
				t.Errorf("estimate flags %q ask for other GPUs than the job's %q", est, want)
			}
		})
	}
}

// TestConcurrentGangSubmitAndDelete stresses GangMu under the race detector:
// many independent 2-member groups are submitted through /createPodGroup and
// then deleted concurrently. Each group has a distinct JID, so exactly one
// scancel per group is expected. This exercises the concurrent Create/Delete
// paths that in production are driven by separate net/http goroutines,
// verifying the lock ordering (narrow scancel decision, unlocked cleanup) has
// no data race and the refcount decision stays correct under contention.
func TestConcurrentGangSubmitAndDelete(t *testing.T) {
	h, _, scancelCount := newHandlerWithStubs(t, "0") // jid overridden per-group below

	const gangs = 25
	// Per-group UNIQUE sbatch JID with no shared-state race in the stub: use a
	// nanosecond timestamp as the job id. Submissions are serialized by GangMu,
	// so every group ends up with its own JID and the refcount/scancel
	// accounting is exact.
	bin := filepath.Dir(h.Config.Sbatchpath)
	h.Config.Sbatchpath = writeStubScript(t, bin, "sbatch",
		"echo \"Submitted batch job $(date +%s%N)\"")

	errCh := make(chan error, gangs*3)
	done := make(chan struct{}, gangs)
	for g := 0; g < gangs; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			gang := "g" + strconv.Itoa(g)
			hUID := gang + "-h"
			wUID := gang + "-w"
			req := PodGroupCreateRequest{GroupUID: gang, Pods: []commonIL.RetrievedPodData{
				{Pod: newGangPod(hUID, nil)}, {Pod: newGangPod(wUID, nil)},
			}}
			if _, cerr := h.createPodGroup(h.Ctx, req); cerr != nil {
				errCh <- cerr.err
				return
			}
			// Delete both members concurrently within the group.
			var wg sync.WaitGroup
			for _, uid := range []string{hUID, wUID} {
				wg.Add(1)
				go func(uid string) {
					defer wg.Done()
					if err := deletePod(h, newGangPod(uid, nil)); err != nil {
						errCh <- err
					}
				}(uid)
			}
			wg.Wait()
		}(g)
	}
	for i := 0; i < gangs; i++ {
		<-done
	}
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent gang op errored: %v", err)
		}
	}

	// Exactly one scancel per group (each group has a distinct JID; the last of
	// its two deletes issues the single scancel).
	if got := scancelCalls(t, scancelCount); got != gangs {
		t.Errorf("scancel calls = %d, want %d (one per group)", got, gangs)
	}
	// All JIDs cleared and GangTable drained.
	if len(*h.JIDs) != 0 {
		t.Errorf("JIDs not fully cleared: %d remaining", len(*h.JIDs))
	}
	if len(h.GangTable) != 0 {
		t.Errorf("GangTable not drained: %d entries remaining", len(h.GangTable))
	}
}

// --- liveness of a submitted gang ---------------------------------------------
//
// A group sent again under the name of a submitted one, with new pod UIDs, is
// submitted afresh only when the old job is gone from SLURM: resubmitting over
// a live job would put two allocations on the same group.

// slurmJobGone classifies squeue output; table locks the liveness contract.
func TestSlurmJobGoneClassifier(t *testing.T) {
	// rc matters: go-execute v1 returns err=nil for ANY nonzero exit (the error
	// lives only in ExecResult.ExitCode), so a signal-killed ssh shim (exit
	// 143/255, empty stdout AND stderr) must NOT be classified gone - that
	// would drop a live gang and double-submit (found by adversarial review,
	// reproduced against go-execute v0.6.0).
	cases := []struct {
		name   string
		stdout string
		stderr string
		rc     int
		gone   bool
	}{
		{"invalid job id (aged out of controller)", "", "slurm_load_jobs error: Invalid job id specified\n", 1, true},
		{"running", "R\n", "", 0, false},
		{"pending", "PD\n", "", 0, false},
		{"completing", "CG\n", "", 0, false},
		{"configuring", "CF\n", "", 0, false},
		{"suspended", "S\n", "", 0, false},
		{"failed", "F\n", "", 0, true},
		{"completed", "CD\n", "", 0, true},
		{"cancelled", "CA\n", "", 0, true},
		{"timeout", "TO\n", "", 0, true},
		{"node fail", "NF\n", "", 0, true},
		{"out of memory", "OOM\n", "", 0, true},
		{"empty output with CLEAN exit means unknown job", "", "", 0, true},
		{"signal-killed ssh (rc 143, silent) is NOT gone", "", "", 143, false},
		{"ssh remote death (rc 255, silent) is NOT gone", "", "", 255, false},
		{"nonzero exit distrusts stdout too (partial pipe output)", "F\n", "", 137, false},
		{"transient ssh shim failure is NOT gone", "", "ssh: connect to host login.example.org port 22: Connection refused\n", 255, false},
		{"unrecognized state treated alive (never double-submit)", "XX\n", "", 0, false},
		{"padded squeue -O output", "R               \n", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := slurmJobGone(c.stdout, c.stderr, c.rc); got != c.gone {
				t.Errorf("slurmJobGone(%q, %q, %d) = %v, want %v", c.stdout, c.stderr, c.rc, got, c.gone)
			}
		})
	}
}

// Native per-rank compute-node discovery: EACH rank records ITS OWN node into
// ITS OWN member dir (inside the rank's srun, so `hostname -f` runs on the
// rank's node, not the batch node). This is what a worker's exposed-ports
// shadow tunnel needs - in a --nodes=N gang the worker runs on a DIFFERENT
// node than the head.
func TestGangScriptWritesPerRankComputeNode(t *testing.T) {
	dataRoot := t.TempDir() + string(os.PathSeparator)
	config := SlurmConfig{BashPath: testBashPath, Commandprefix: testModuleLoad}
	entry, ordered := gangOf("g1", mkMember(t, dataRoot, "h", nil), mkMember(t, dataRoot, "w", nil))
	s := gangScript(t, config, entry, ordered)

	// Quoting note: the rank inner goes through nested shellescape (the path is
	// quoted inside the inner, the inner is quoted into bash -c), so assert the
	// SEMANTIC parts - the member-specific target path and the hostname write -
	// rather than exact quote bytes.
	firstSrun := strings.Index(s, "srun --overlap")
	if firstSrun < 0 {
		t.Fatalf("gang script has no per-rank srun\n---\n%s", s)
	}
	for _, uid := range []string{"h", "w"} {
		want := "default-" + uid + "/compute-node"
		idx := strings.Index(s, want)
		if idx < 0 {
			t.Errorf("gang script must write rank compute-node into member %s's OWN dir (%q)\n---\n%s", uid, want, s)
			continue
		}
		// The write must be INSIDE the rank's srun bash -c (i.e. AFTER the first
		// srun of the script), not at the batch level: at batch level `hostname`
		// reports the batch host (nodes[0]) for every member, which is wrong for
		// workers.
		if idx < firstSrun {
			t.Errorf("member %s compute-node write must run inside its rank srun, not at batch level\n---\n%s", uid, s)
		}
	}
	if got := strings.Count(s[firstSrun:], "hostname -f > "); got != 2 {
		t.Errorf("expected one per-rank hostname write per member (2), got %d\n---\n%s", got, s)
	}
}

// Deterministic head discovery for shadow/tunnel pods: the gang batch script
// (which runs on nodes[0] = the head's node) must publish the head node under
// a well-known gang-keyed path <DataRootFolder>/.gang-heads/<ns>-<group>.
// The VK's wstunnel template exposes only name/namespace/labels/annotations
// (NO pod UID, verified against interLink 0.6.1), so a shadow pod cannot
// resolve the head member's per-UID dir - but it CAN fetch this file by the
// group's name.
func TestGangScriptPublishesGangHeadDiscoveryFile(t *testing.T) {
	dataRoot := t.TempDir() + string(os.PathSeparator)
	config := SlurmConfig{BashPath: testBashPath, DataRootFolder: dataRoot, Commandprefix: testModuleLoad}
	entry, ordered := gangOf("g1", mkMember(t, dataRoot, "h", nil), mkMember(t, dataRoot, "w", nil))
	s := gangScript(t, config, entry, ordered)

	headFile := dataRoot + ".gang-heads/default-g1"
	idx := strings.Index(s, headFile)
	if idx < 0 {
		t.Fatalf("gang script must publish the head node to %q\n---\n%s", headFile, s)
	}
	// Must run at the BATCH level (before any srun): the batch script executes on
	// nodes[0], which is rank 0's (the head's) node, so `hostname -f` there IS the
	// head node. Inside a rank srun it could be any member's node.
	if firstSrun := strings.Index(s, "srun --overlap"); firstSrun >= 0 && idx > firstSrun {
		t.Errorf("gang-head discovery write must be at the batch level, before the per-rank sruns\n---\n%s", s)
	}
	if !strings.Contains(s, "mkdir -p ") {
		t.Error("the .gang-heads dir must be created before writing into it")
	}
}

// --- MPI-mode + mpi-env hook tests ------------------------------------------

// mkGangMemberWithRC builds a gang member carrying ONE fully-rendered
// ContainerCommand (like Create.go's runtime_command_pod[0]): its runtimeCommand
// already includes the SIF path at the end (mirroring prepareRuntimeCommand +
// image), plus an explicit containerCommand/containerArgs so the MPI-mode
// reconstruction has something to rebuild (exactly as runCtn does).
func mkGangMemberWithRC(t *testing.T, dataRoot, uid string, extra map[string]string, cmd, args []string) *BufferedMember {
	t.Helper()
	m := mkMember(t, dataRoot, uid, extra)
	m.RuntimeCommands = []ContainerCommand{{
		containerName: testContainer,
		// runtimeCommand mirrors Create.go: singularity exec <opts> <mounts> <SIF>.
		// The SIF path is the LAST element (rc.runtimeCommand already includes it).
		runtimeCommand:   []string{singularityRuntime, string(ProbeTypeExec), testNV, "--no-home", "", "", "/gpfs/images/itwinai.sif"},
		containerCommand: cmd,
		containerArgs:    args,
		containerImage:   "/gpfs/images/itwinai.sif",
	}}
	return m
}

// REGRESSION (critical): with gang-mode ABSENT, produceGangSLURMScript output is
// UNCHANGED from the per-rank behavior. Covers a KubeRay-style gang (head+worker,
// Ray env) AND a torch-style gang. Every per-rank srun --overlap loop, the
// readiness barrier, and the DRIVER_RC tail must all still be present, and NO MPI
// launcher (`srun --mpi=`) must appear.
func TestProduceGangSLURMScriptDefaultModeUnchanged(t *testing.T) {
	run := func(t *testing.T, extra map[string]string) string {
		dataRoot := t.TempDir() + string(os.PathSeparator)
		config := SlurmConfig{BashPath: testBashPath, Commandprefix: testModuleLoad}
		entry, ordered := gangOf("g1",
			mkGangMemberWithRC(t, dataRoot, "h", extra, []string{testPython}, []string{testTrainScript}),
			mkGangMemberWithRC(t, dataRoot, "w", extra, []string{testPython}, []string{testTrainScript}))
		return gangScript(t, config, entry, ordered)
	}

	// KubeRay-style: head+worker, Ray env is present in the coordination block.
	t.Run("kuberay", func(t *testing.T) {
		s := run(t, nil)
		// Per-rank loop for EACH member: 2 member sruns + 1 barrier probe srun = 3.
		if got := strings.Count(s, "srun --overlap --nodes=1 --ntasks=1"); got != 3 {
			t.Errorf("default mode must emit one per-rank srun per member plus the barrier probe (3), got %d\n---\n%s", got, s)
		}
		// Both member dirs must be routed by their own per-rank srun.
		if !strings.Contains(s, "default-h/job.out") || !strings.Contains(s, "default-w/job.out") {
			t.Errorf("default mode must route each member's -o into its own dir\n---\n%s", s)
		}
		// Readiness barrier still present.
		if !strings.Contains(s, "/dev/tcp/$head_ip/$MASTER_PORT") {
			t.Errorf("default mode must keep the readiness barrier\n---\n%s", s)
		}
		// DRIVER_RC tail still present.
		for _, want := range []string{"DRIVER_RC=0", "wait \"${RANK_PIDS[0]}\"", "exit \"$DRIVER_RC\""} {
			if !strings.Contains(s, want) {
				t.Errorf("default mode must keep the DRIVER_RC tail (%q)\n---\n%s", want, s)
			}
		}
		// Ray coordination env present (RAY_ADDRESS is emitted for all gangs today).
		if !strings.Contains(s, "RAY_ADDRESS=\"$head_ip:6379\"") {
			t.Errorf("default mode must keep the RAY_ADDRESS coordination env\n---\n%s", s)
		}
		// No MPI launcher whatsoever.
		if strings.Contains(s, "srun --mpi=") {
			t.Errorf("default mode must NOT emit an MPI launcher\n---\n%s", s)
		}
		if strings.Contains(s, "SRUN_CPUS_PER_TASK") {
			t.Errorf("default mode must NOT emit the MPI SRUN_CPUS_PER_TASK export\n---\n%s", s)
		}
	})

	// torch-style: same shape, no Ray specifics needed; the per-rank + barrier +
	// DRIVER_RC contract is identical (torch consumes MASTER_ADDR/RANK/WORLD_SIZE).
	t.Run("torch", func(t *testing.T) {
		s := run(t, map[string]string{"slurm-job.vk.io/singularity-options": testNV})
		// 2 member sruns + 1 barrier probe srun = 3.
		if got := strings.Count(s, "srun --overlap --nodes=1 --ntasks=1"); got != 3 {
			t.Errorf("torch default mode must emit one per-rank srun per member plus the barrier probe (3), got %d\n---\n%s", got, s)
		}
		if !strings.Contains(s, "/dev/tcp/$head_ip/$MASTER_PORT") {
			t.Errorf("torch default mode must keep the readiness barrier\n---\n%s", s)
		}
		if !strings.Contains(s, "export MASTER_ADDR") || !strings.Contains(s, "WORLD_SIZE=2") {
			t.Errorf("torch default mode must keep MASTER_ADDR/WORLD_SIZE coordination env\n---\n%s", s)
		}
		if strings.Contains(s, "srun --mpi=") {
			t.Errorf("torch default mode must NOT emit an MPI launcher\n---\n%s", s)
		}
	})
}

// mpi-env hook: SINGULARITYENV_<K> exports appear for a given annotation, in BOTH
// modes; and are ABSENT when the annotation is unset.
func TestGangScriptMPIEnvHook(t *testing.T) {
	renderWith := func(t *testing.T, extra map[string]string) string {
		dataRoot := t.TempDir() + string(os.PathSeparator)
		config := SlurmConfig{BashPath: testBashPath, Commandprefix: testModuleLoad}
		entry, ordered := gangOf("g1",
			mkGangMemberWithRC(t, dataRoot, "h", extra, []string{testPython}, []string{testScript}),
			mkGangMemberWithRC(t, dataRoot, "w", extra, []string{testPython}, []string{testScript}))
		return gangScript(t, config, entry, ordered)
	}

	// Present in DEFAULT (per-rank) mode.
	t.Run("default-mode", func(t *testing.T) {
		s := renderWith(t, map[string]string{testMPIEnvAnnotation: "NCCL_DEBUG=INFO;UCX_TLS=rc,ud"})
		for _, want := range []string{
			`export SINGULARITYENV_NCCL_DEBUG=INFO`,
			`export SINGULARITYENV_UCX_TLS=`, // value shellescaped; prefix is enough
		} {
			if !strings.Contains(s, want) {
				t.Errorf("default mode must emit mpi-env export %q\n---\n%s", want, s)
			}
		}
		// The exports must precede the launch section (per-rank srun).
		if e, sr := strings.Index(s, "SINGULARITYENV_NCCL_DEBUG"), strings.Index(s, "srun --overlap"); e < 0 || sr < 0 || e > sr {
			t.Errorf("mpi-env exports must precede the launch section: env@%d srun@%d\n---\n%s", e, sr, s)
		}
	})

	// Absent when the annotation is unset.
	t.Run("unset", func(t *testing.T) {
		s := renderWith(t, nil)
		if strings.Contains(s, "SINGULARITYENV_") {
			t.Errorf("no mpi-env annotation must emit NO SINGULARITYENV_ exports\n---\n%s", s)
		}
	})

	// Malformed/empty pairs are skipped, valid ones still emitted.
	t.Run("skips-malformed", func(t *testing.T) {
		s := renderWith(t, map[string]string{testMPIEnvAnnotation: "NCCL_DEBUG=INFO;;NOEQUALS;=novalue;GOOD=1"})
		if !strings.Contains(s, `export SINGULARITYENV_NCCL_DEBUG=INFO`) || !strings.Contains(s, `export SINGULARITYENV_GOOD=1`) {
			t.Errorf("valid pairs must still be emitted alongside malformed ones\n---\n%s", s)
		}
		if strings.Contains(s, "SINGULARITYENV_NOEQUALS") || strings.Contains(s, "SINGULARITYENV_=") {
			t.Errorf("malformed pairs (no '=', empty key) must be skipped\n---\n%s", s)
		}
	})

	// Keys that are not valid shell env-var names are dropped BEFORE the unquoted
	// `export SINGULARITYENV_<K>` interpolation (hardening the key side; the value
	// is already shellescaped). A hostile key must not reach the export line.
	t.Run("rejects-unsafe-key", func(t *testing.T) {
		s := renderWith(t, map[string]string{testMPIEnvAnnotation: "GOOD_1=ok;BAD-KEY=x;E vil=y;X;rm -rf /=z"})
		if !strings.Contains(s, `export SINGULARITYENV_GOOD_1=ok`) {
			t.Errorf("valid key GOOD_1 must still be emitted\n---\n%s", s)
		}
		for _, bad := range []string{"BAD-KEY", "rm -rf", "E vil", "SINGULARITYENV_X="} {
			if strings.Contains(s, bad) {
				t.Errorf("unsafe key fragment %q must not reach the script\n---\n%s", bad, s)
			}
		}
	})
}

// Only the head runs its pre-exec, inside its own rank and with its own socket
// directory: the shadow reaches the head's node only.
func TestGangHeadRunsItsPreExec(t *testing.T) {
	dataRoot := t.TempDir() + string(os.PathSeparator)
	config := SlurmConfig{BashPath: testBashPath, SocketDirRoot: socketTestRoot}
	pre := map[string]string{preExecAnnotation: "echo set-up-the-tunnel"}
	head, worker := socketTestUID, "7d1e2f30-4a5b-4c6d-8e7f-90a1b2c3d4e5"
	entry, ordered := gangOf("g1", mkMember(t, dataRoot, head, pre), mkMember(t, dataRoot, worker, pre))
	script := gangScript(t, config, entry, ordered)

	if n := strings.Count(script, "echo set-up-the-tunnel"); n != 1 {
		t.Fatalf("the pre-exec runs %d times, want once\n%s", n, script)
	}
	rank0, rank1, _ := strings.Cut(script, "# rank 1 (")
	if !strings.Contains(rank0, "export INTERLINK_SOCKET_DIR="+socketDirFor(socketTestRoot, head)+"\necho set-up-the-tunnel\nexec ") {
		t.Errorf("rank 0 does not run the pre-exec with its socket directory before job.sh\n%s", rank0)
	}
	if strings.Contains(rank1, "set-up-the-tunnel") {
		t.Errorf("a worker runs the pre-exec\n%s", rank1)
	}
}
