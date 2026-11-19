package slurm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
)

const testGroup = "wl-uid"

// podGroupRequest is a pod group of n pods in rank order.
func podGroupRequest(n int) PodGroupCreateRequest {
	return podGroupRequestOf(testGroup, n)
}

// podGroupRequestOf is podGroupRequest with pod UIDs <prefix>-<rank>.
func podGroupRequestOf(prefix string, n int) PodGroupCreateRequest {
	req := PodGroupCreateRequest{GroupUID: testGroup}
	for rank := 0; rank < n; rank++ {
		req.Pods = append(req.Pods, commonIL.RetrievedPodData{Pod: newGangPod(prefix+"-"+strconv.Itoa(rank), nil)})
	}
	return req
}

// newPodGroupHandler is newHandlerWithStubs with an sbatch that counts its calls
// in the returned file.
func newPodGroupHandler(t *testing.T, sbatchBody string) (*SidecarHandler, string, string, string) {
	t.Helper()
	h, dataRoot, scancelCount := newHandlerWithStubs(t, "42")
	sbatchCount := filepath.Join(filepath.Dir(h.Config.Sbatchpath), "sbatch.count")
	h.Config.Sbatchpath = writeStubScript(t, filepath.Dir(h.Config.Sbatchpath), "sbatch-counted",
		"echo x >> "+sbatchCount+"\n"+sbatchBody)
	return h, dataRoot, scancelCount, sbatchCount
}

func postPodGroup(t *testing.T, h *SidecarHandler, req PodGroupCreateRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.CreatePodGroupHandler(rec, httptest.NewRequest(http.MethodPost, "/createPodGroup", bytes.NewReader(body)))
	return rec
}

// createdJIDs decodes a /createPodGroup answer into the job ID of each pod, in
// order.
func createdJIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, body %q", rec.Code, rec.Body.String())
	}
	var created []CreateStruct
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	jids := make([]string, len(created))
	for i, c := range created {
		jids[i] = c.PodJID
	}
	return jids
}

// rankUID is the UID podGroupRequest gives to a rank.
func rankUID(rank int) string { return testGroup + "-" + strconv.Itoa(rank) }

// One call submits the whole group as one job, ranks in the order of the
// request; a retried call gets the same job without a second sbatch; the
// deletes of the members cancel the job once.
func TestCreatePodGroupSubmitsOneJob(t *testing.T) {
	h, dataRoot, scancelCount, sbatchCount := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
	req := podGroupRequest(3)

	for attempt := 0; attempt < 2; attempt++ {
		rec := postPodGroup(t, h, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: code %d, body %q", attempt, rec.Code, rec.Body.String())
		}
		var created []CreateStruct
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if len(created) != 3 {
			t.Fatalf("attempt %d: %d answers, want 3", attempt, len(created))
		}
		for i, c := range created {
			if want := rankUID(i); c.PodUID != want || c.PodJID != "42" {
				t.Fatalf("attempt %d: answer %d = %+v, want %s with job 42", attempt, i, c, want)
			}
		}
	}
	if n := countLines(t, sbatchCount); n != 1 {
		t.Fatalf("sbatch ran %d times, want 1", n)
	}

	raw, err := os.ReadFile(memberDir(dataRoot, rankUID(0)) + "/job.slurm")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for _, want := range []string{
		"#SBATCH --nodes=3\n",
		"#SBATCH --job-name=" + testGroup + "\n",
		"# rank 0 (head) pod " + testGroup + "-0\n",
		"# rank 1 (worker) pod " + testGroup + "-1\n",
		"# rank 2 (worker) pod " + testGroup + "-2\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("job.slurm lacks %q\n---\n%s", want, script)
		}
	}
	for i := 0; i < 3; i++ {
		uid := rankUID(i)
		if j := (*h.JIDs)[uid]; j == nil || j.JID != "42" || !j.Gang {
			t.Fatalf("JIDs[%s] = %+v, want gang job 42", uid, j)
		}
	}
	if e := h.GangTable[testGroup]; e == nil || !e.Submitted || e.JID != "42" || len(e.Members) != 3 {
		t.Fatalf("gang entry %+v, want submitted job 42 with 3 members", e)
	}

	for _, data := range req.Pods {
		if err := deletePod(h, data.Pod); err != nil {
			t.Fatal(err)
		}
	}
	if n := scancelCalls(t, scancelCount); n != 1 {
		t.Fatalf("scancel ran %d times, want 1", n)
	}
	if _, ok := h.GangTable[testGroup]; ok {
		t.Fatal("the gang entry outlived its members")
	}
}

// fakeEnrootEnv stands for enroot: start writes each --env to a file as enroot's
// runtime::_do_environ does, formats the file twice with common::envfmt of enroot
// 4.2.1 (src/common.sh), as _do_environ and _do_hooks do, and prints it. The other
// commands do nothing.
const fakeEnrootEnv = `[ "$1" = start ] || exit 0
shift
env=$(mktemp)
trap 'rm -f "$env"' EXIT
while [ $# -gt 1 ]; do
  if [ "$1" = --env ]; then
    printf '%s\n' "$2" | awk '{ print /=/ ? $0 : $0"="ENVIRON[$0] }' >> "$env"
    shift
  fi
  shift
done
for pass in 1 2; do
  sed -i -e 's/^[[:space:]]\+//' -e '/^#\|^$/d' -e '/^[[:alpha:]_][[:alnum:]_]*=/!d' -e '/^ENROOT_/d' \
    -e 's/^\([[:alpha:]_][[:alnum:]_]*\)=[\"\x27]\(.*\)[\"\x27][[:space:]]*$/\1=\2/' "$env"
done
cat "$env"`

// enroot starts a container without the host environment: every container of a
// gang member gets the rank's variables by name, which enroot resolves in the
// rank's environment, and the gang's mpi-env with its values. A single pod gets
// none of them.
func TestCreatePodGroupEnrootPassesRankEnv(t *testing.T) {
	h, dataRoot, _, _ := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
	h.Config.ContainerRuntime = enrootRuntime
	h.Config.EnrootPath = writeStubScript(t, t.TempDir(), enrootRuntime, fakeEnrootEnv)
	req := podGroupRequest(2)
	req.Pods[0].Pod.Annotations = map[string]string{testMPIEnvAnnotation: "NCCL_DEBUG=INFO;RANK=7;" +
		`DQ="q";BS=\a\;SPECIAL=it's "q" $HOME \x;NL=a` + "\n" + "b"}
	createdJIDs(t, postPodGroup(t, h, req))

	raw, err := os.ReadFile(memberDir(dataRoot, rankUID(0)) + "/job.slurm")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if strings.Contains(script, "export NCCL_DEBUG=") {
		t.Errorf("job.slurm exports the mpi-env in the batch shell\n%s", script)
	}
	// Every variable gangEnrootEnv passes by name is one the rank's srun sets.
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "--export=ALL,") {
			continue
		}
		for _, name := range gangRankEnv {
			if !strings.Contains(line, ","+name+"=") {
				t.Errorf("the rank's srun does not set %s: %s", name, line)
			}
		}
	}

	worker := memberDir(dataRoot, rankUID(1))
	raw, err = os.ReadFile(worker + "/job.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := " --env RANK --env WORLD_SIZE --env MASTER_ADDR --env MASTER_PORT --env RAY_ADDRESS --env NCCL_DEBUG=INFO "
	if !strings.Contains(string(raw), want) {
		t.Fatalf("the worker's enroot start lacks %q\n%s", want, raw)
	}

	// The worker's job.sh, run as its rank's srun runs it, with a host value of an
	// mpi-env name that the container must not get.
	cmd := exec.Command(testBashPath, "job.sh")
	cmd.Dir = worker
	cmd.Env = append(os.Environ(), "RANK=1", "WORLD_SIZE=2", "MASTER_ADDR=10.0.0.1", "MASTER_PORT=29500",
		"RAY_ADDRESS=10.0.0.1:29500", "NCCL_DEBUG=WARN")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("job.sh: %v\n%s", err, out)
	}
	raw, err = os.ReadFile(worker + "/run-" + testGangContainer + ".out")
	if err != nil {
		t.Fatal(err)
	}
	got := "\n" + string(raw)
	for _, want := range []string{"RANK=1", "WORLD_SIZE=2", "MASTER_ADDR=10.0.0.1", "MASTER_PORT=29500",
		"RAY_ADDRESS=10.0.0.1:29500", "NCCL_DEBUG=INFO", `DQ="q"`, `BS=\a\`, `SPECIAL=it's "q" $HOME \x`} {
		if !strings.Contains(got, "\n"+want+"\n") {
			t.Errorf("the container does not get %s\n%s", want, raw)
		}
	}
	for _, unwanted := range []string{"\nRANK=7\n", "\nNL="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the container gets %q\n%s", strings.TrimSpace(unwanted), raw)
		}
	}

	single := newGangPod("single", nil)
	if rec := submitPod(t, h, single); rec.Code != http.StatusOK {
		t.Fatalf("single pod: code %d, body %q", rec.Code, rec.Body.String())
	}
	raw, err = os.ReadFile(memberDir(dataRoot, "single") + "/job.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "--env RANK") {
		t.Errorf("a single pod's enroot start gets the rank variables\n%s", raw)
	}
}

// Singularity keeps the host environment: a gang member's job.sh passes nothing.
func TestCreatePodGroupSingularityPassesNoEnv(t *testing.T) {
	h, dataRoot, _, _ := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
	req := podGroupRequest(2)
	req.Pods[0].Pod.Annotations = map[string]string{testMPIEnvAnnotation: "NCCL_DEBUG=INFO"}
	createdJIDs(t, postPodGroup(t, h, req))

	for rank := 0; rank < 2; rank++ {
		raw, err := os.ReadFile(memberDir(dataRoot, rankUID(rank)) + "/job.sh")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "--env RANK") || strings.Contains(string(raw), "--env NCCL_DEBUG") {
			t.Errorf("rank %d: singularity is given the gang's variables\n%s", rank, raw)
		}
	}
}

func TestCreatePodGroupRefusals(t *testing.T) {
	sameUID := podGroupRequest(2)
	sameUID.Pods[1].Pod.UID = sameUID.Pods[0].Pod.UID
	noUID := podGroupRequest(2)
	noUID.Pods[1].Pod.UID = ""
	noGroup := podGroupRequest(2)
	noGroup.GroupUID = ""

	tests := []struct {
		name    string
		req     PodGroupCreateRequest
		gangOff bool
		want    int
	}{
		{name: "gang scheduling off", req: podGroupRequest(2), gangOff: true, want: http.StatusBadRequest},
		{name: "no group UID", req: noGroup, want: http.StatusBadRequest},
		{name: "no pods", req: PodGroupCreateRequest{GroupUID: testGroup}, want: http.StatusBadRequest},
		{name: "two pods with one UID", req: sameUID, want: http.StatusBadRequest},
		{name: "pod without UID", req: noUID, want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, sbatchCount := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
			h.Config.GangSchedulingEnabled = !tt.gangOff
			if rec := postPodGroup(t, h, tt.req); rec.Code != tt.want {
				t.Fatalf("code %d, want %d; body %q", rec.Code, tt.want, rec.Body.String())
			}
			if n := countLines(t, sbatchCount); n != 0 {
				t.Fatalf("sbatch ran %d times", n)
			}
		})
	}
}

// A refused sbatch answers 503 for what trying again later can fix and 400 for a
// job Slurm refuses, with sbatch's text, and leaves nothing behind, so a later
// call starts afresh. /create keeps answering 504 for the same refusal.
func TestCreatePodGroupSbatchRefused(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		rc     int
		want   int
	}{
		{"ssh failed", "ssh: connect to host login.hpc-a.example.org port 22: Connection timed out", 255, http.StatusServiceUnavailable},
		{"QOS submit limit", "sbatch: error: QOSMaxSubmitJobPerUserLimit\nsbatch: error: Batch job submission failed: Job violates accounting/QOS policy (job submit limit, user's size and/or time limits)", 1, http.StatusServiceUnavailable},
		{"association submit limit", "sbatch: error: AssocMaxSubmitJobLimit\nsbatch: error: Batch job submission failed: Job violates accounting/QOS policy (job submit limit, user's size and/or time limits)", 1, http.StatusServiceUnavailable},
		{"submit limit", "sbatch: error: MaxSubmitJobsLimit", 1, http.StatusServiceUnavailable},
		{"controller down", "sbatch: error: Batch job submission failed: Unable to contact slurm controller (connect failure)", 1, http.StatusServiceUnavailable},
		{"controller busy", "sbatch: error: Batch job submission failed: Socket timed out on send/recv operation", 1, http.StatusServiceUnavailable},
		{"controller not accepting", "sbatch: error: Slurm temporarily unable to accept job, sleeping and retrying", 1, http.StatusServiceUnavailable},
		{"invalid account", "sbatch: error: Batch job submission failed: Invalid account or account/partition combination specified", 1, http.StatusBadRequest},
		{"invalid partition", "sbatch: error: invalid partition specified: nope\nsbatch: error: Batch job submission failed: Invalid partition name specified", 1, http.StatusBadRequest},
		{"invalid QOS", "sbatch: error: Batch job submission failed: Invalid qos specification", 1, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(tt.stderr+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			firstLine, _, _ := strings.Cut(tt.stderr, "\n")
			h, dataRoot, _, _ := newPodGroupHandler(t, "cat "+filepath.Join(dir, "stderr")+" >&2; exit "+strconv.Itoa(tt.rc))

			rec := postPodGroup(t, h, podGroupRequest(2))
			if rec.Code != tt.want || !strings.Contains(rec.Body.String(), firstLine) {
				t.Fatalf("code %d %q, want %d with sbatch's text", rec.Code, rec.Body.String(), tt.want)
			}
			for i := 0; i < 2; i++ {
				if _, err := os.Stat(memberDir(dataRoot, rankUID(i))); !os.IsNotExist(err) {
					t.Fatalf("rank %d directory left behind: %v", i, err)
				}
			}
			if len(*h.JIDs) != 0 || len(h.GangTable) != 0 {
				t.Fatalf("state left behind: JIDs %v, gangs %v", *h.JIDs, h.GangTable)
			}

			if rec := submitPod(t, h, newGangPod("uid-one", nil)); rec.Code != http.StatusGatewayTimeout {
				t.Fatalf("/create: code %d %q, want 504 as before", rec.Code, rec.Body.String())
			}

			h.Config.Sbatchpath = writeStubScript(t, t.TempDir(), "sbatch", "echo 'Submitted batch job 43'")
			if rec := postPodGroup(t, h, podGroupRequest(2)); rec.Code != http.StatusOK {
				t.Fatalf("retry: code %d, body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

// An sbatch that ran may have started a job even when the group fails: its job
// ID was not printed, or not recorded on every pod. The group stays known, a
// retry is refused rather than submitted again, and the deletes of its pods
// forget it; the last one cancels the job when its ID is known.
func TestCreatePodGroupKeepsEntryAfterSbatchRan(t *testing.T) {
	tests := []struct {
		name       string
		sbatchBody func(dataRoot string) string
		jid        string
	}{
		{"no job ID", func(string) string { return "echo 'queued, no id'" }, ""},
		// The pods' directories go missing after sbatch ran: the job ID cannot be
		// stamped on the head, or on the worker after the head.
		{"head not stamped", func(dataRoot string) string {
			return "rm -rf " + memberDir(dataRoot, rankUID(0)) + "; echo 'Submitted batch job 77'"
		}, "77"},
		{"worker not stamped", func(dataRoot string) string {
			return "rm -rf " + memberDir(dataRoot, rankUID(1)) + "; echo 'Submitted batch job 77'"
		}, "77"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, scancelCount, _ := newPodGroupHandler(t, "")
			sbatchCount := filepath.Join(filepath.Dir(h.Config.Sbatchpath), "sbatch.count")
			h.Config.Sbatchpath = writeStubScript(t, filepath.Dir(h.Config.Sbatchpath), "sbatch-ran",
				"echo x >> "+sbatchCount+"\n"+tt.sbatchBody(h.Config.DataRootFolder))

			if rec := postPodGroup(t, h, podGroupRequest(2)); rec.Code == http.StatusOK {
				t.Fatalf("a group whose job was not recorded was answered: %q", rec.Body.String())
			}
			if e := h.GangTable[testGroup]; e == nil || e.Submitted || e.JID != tt.jid || len(e.Members) != 2 {
				t.Fatalf("gang entry %+v, want the 2 members kept with job %q", e, tt.jid)
			}
			if len(*h.JIDs) != 0 {
				t.Fatalf("JIDs %v, want no pod holding the job", *h.JIDs)
			}
			if rec := postPodGroup(t, h, podGroupRequest(2)); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("retry: code %d %q, want 503", rec.Code, rec.Body.String())
			}
			if n := countLines(t, sbatchCount); n != 1 {
				t.Fatalf("sbatch ran %d times, want 1", n)
			}

			if err := deletePod(h, newGangPod(rankUID(0), nil)); err != nil {
				t.Fatal(err)
			}
			if n := scancelCalls(t, scancelCount); n != 0 {
				t.Fatalf("scancel ran %d times before the last pod was deleted", n)
			}
			if err := deletePod(h, newGangPod(rankUID(1), nil)); err != nil {
				t.Fatal(err)
			}
			wantCancels := 0
			if tt.jid != "" {
				wantCancels = 1
			}
			if n := scancelCalls(t, scancelCount); n != wantCancels {
				t.Fatalf("scancel ran %d times, want %d", n, wantCancels)
			}
			if wantCancels == 1 {
				if b, _ := os.ReadFile(scancelCount); strings.TrimSpace(string(b)) != tt.jid {
					t.Fatalf("scancel got %q, want job %s", b, tt.jid)
				}
			}
			if _, ok := h.GangTable[testGroup]; ok {
				t.Fatal("the gang entry outlived its members")
			}
		})
	}
}

// Pods with slurm-job.vk.io/job-workdir keep their job files there, as /create
// does: the gang job runs every member's job.sh from its working directory, the
// status and the delete find it there.
func TestCreatePodGroupJobWorkdir(t *testing.T) {
	h, dataRoot, scancelCount, _ := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
	base := t.TempDir()
	req := podGroupRequest(2)
	for i := range req.Pods {
		req.Pods[i].Pod.Annotations = map[string]string{"slurm-job.vk.io/job-workdir": base}
	}
	createdJIDs(t, postPodGroup(t, h, req))

	workDir := func(rank int) string { return filepath.Join(base, testNamespace+"-"+rankUID(rank)) }
	raw, err := os.ReadFile(workDir(0) + "/job.slurm")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if !strings.Contains(script, "#SBATCH --output="+workDir(0)+"/job.out\n") {
		t.Errorf("job.slurm does not write the head's log to its working directory\n---\n%s", script)
	}
	for i := 0; i < 2; i++ {
		jobSh := workDir(i) + "/job.sh"
		if !strings.Contains(script, jobSh) {
			t.Errorf("job.slurm does not run %s\n---\n%s", jobSh, script)
		}
		if _, err := os.Stat(jobSh); err != nil {
			t.Errorf("rank %d: %v", i, err)
		}
		if j := (*h.JIDs)[rankUID(i)]; j == nil || j.WorkDir != workDir(i) {
			t.Errorf("JIDs[%s] = %+v, want WorkDir %s", rankUID(i), j, workDir(i))
		}
		if b, err := os.ReadFile(memberDir(dataRoot, rankUID(i)) + "/WorkDir.path"); err != nil || string(b) != workDir(i) {
			t.Errorf("rank %d WorkDir.path = %q, %v", i, b, err)
		}
	}

	for _, data := range req.Pods {
		if err := deletePod(h, data.Pod); err != nil {
			t.Fatal(err)
		}
	}
	if n := scancelCalls(t, scancelCount); n != 1 {
		t.Fatalf("scancel ran %d times, want 1", n)
	}
	for i := 0; i < 2; i++ {
		for _, dir := range []string{workDir(i), memberDir(dataRoot, rankUID(i))} {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("rank %d: %s left behind: %v", i, dir, err)
			}
		}
	}
}

// THE regression test: a group sent again under the name of a submitted one,
// with NEW pod UIDs (the workload was deleted and recreated), whose job died and
// aged out of the controller, gets a fresh sbatch, not the dead JID.
func TestCreatePodGroupStaleGroupStartsAfresh(t *testing.T) {
	h, dataRoot, scancelCount, _ := newPodGroupHandler(t, "echo 'Submitted batch job 111'")
	if jids := createdJIDs(t, postPodGroup(t, h, podGroupRequestOf("gen1", 2))); jids[0] != "111" {
		t.Fatalf("gen1: jobs %v, want 111", jids)
	}

	// Job 111 dies and ages out: squeue rejects the job id (verbatim Slurm output).
	bin := t.TempDir()
	h.Config.Squeuepath = writeStubScript(t, bin, "squeue",
		`echo "slurm_load_jobs error: Invalid job id specified" >&2; exit 1`)
	sbatchCalls := filepath.Join(bin, "sbatch.count")
	h.Config.Sbatchpath = writeStubScript(t, bin, "sbatch",
		"echo x >> "+sbatchCalls+"\necho \"Submitted batch job 222\"")

	jids := createdJIDs(t, postPodGroup(t, h, podGroupRequestOf("gen2", 2)))
	if jids[0] != "222" || jids[1] != "222" {
		t.Fatalf("gen2: jobs %v, want a fresh job 222", jids)
	}
	for _, uid := range []string{"gen2-0", "gen2-1"} {
		if b, err := os.ReadFile(memberDir(dataRoot, uid) + "/JobID.jid"); err != nil || string(b) != "222" {
			t.Errorf("gen2 member %s JobID.jid on disk = %q err=%v, want 222", uid, string(b), err)
		}
	}
	if n := countLines(t, sbatchCalls); n != 1 {
		t.Errorf("expected exactly one fresh sbatch for gen2, got %d", n)
	}
	if e := h.GangTable[testGroup]; e == nil || e.JID != "222" || len(e.Members) != 2 {
		t.Errorf("gang entry %+v, want gen2's job 222", e)
	}
	if scancelCalls(t, scancelCount) != 0 {
		t.Error("dropping a stale (already dead) gang entry must NOT scancel")
	}
}

// NEW pod UIDs for a group whose job is still ALIVE are refused with 503: the
// running allocation is never resubmitted over, and new pods are not bound to it.
func TestCreatePodGroupRefusesWhileGroupRuns(t *testing.T) {
	h, _, _, sbatchCount := newPodGroupHandler(t, "echo 'Submitted batch job 111'")
	createdJIDs(t, postPodGroup(t, h, podGroupRequestOf("gen1", 2)))

	h.Config.Squeuepath = writeStubScript(t, t.TempDir(), "squeue", `echo "R"`)
	if rec := postPodGroup(t, h, podGroupRequestOf("gen2", 2)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d %q, want 503", rec.Code, rec.Body.String())
	}
	if n := countLines(t, sbatchCount); n != 1 {
		t.Errorf("sbatch ran %d times while the group's job is alive, want 1", n)
	}
}

// A retry of the same pods returns the shared JID WITHOUT consulting squeue: even
// if the job is gone, the pods belong to it and keep its JID (Status handles the
// terminal reporting).
func TestCreatePodGroupRetryIgnoresJobState(t *testing.T) {
	h, _, _, sbatchCount := newPodGroupHandler(t, "echo 'Submitted batch job 111'")
	createdJIDs(t, postPodGroup(t, h, podGroupRequest(2)))

	h.Config.Squeuepath = writeStubScript(t, t.TempDir(), "squeue",
		`echo "slurm_load_jobs error: Invalid job id specified" >&2; exit 1`)
	if jids := createdJIDs(t, postPodGroup(t, h, podGroupRequest(2))); jids[0] != "111" || jids[1] != "111" {
		t.Fatalf("retry: jobs %v, want the shared job 111", jids)
	}
	if n := countLines(t, sbatchCount); n != 1 {
		t.Errorf("sbatch ran %d times, want 1", n)
	}
}

// submitPod sends one pod to /create and returns the answer.
func submitPod(t *testing.T, h *SidecarHandler, pod v1.Pod) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(commonIL.RetrievedPodData{Pod: pod})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.SubmitHandler(rec, httptest.NewRequest(http.MethodPost, "/create", bytes.NewReader(body)))
	return rec
}

// countLines counts the lines of a stub's counter file; none when it is absent.
func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}
