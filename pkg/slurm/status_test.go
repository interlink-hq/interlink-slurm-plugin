package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
)

// TestSlurmStateRegex validates that all expected SLURM state codes, including the
// newly-added TO (TIMEOUT) and OOM (out-of-memory) codes, are matched by the regex
// used in StatusHandler.  It also checks that multi-character codes like ST are not
// shadowed by the single-character S alternative.
func TestSlurmStateRegex(t *testing.T) {
	re := regexp.MustCompile(SlurmStatePattern)

	tests := []struct {
		name       string
		squeueLine string // simulated squeue output: "<exit_code> <state>"
		wantState  string
		wantMatch  bool
	}{
		// Pre-existing states
		{"completed", "0                 CD", "CD", true},
		{"completing", "0                 CG", "CG", true},
		{"failed", "1                 F", "F", true},
		{"pending", "0                 PD", "PD", true},
		{"preempted", "0:15              PR", "PR", true},
		{"running", "0                 R", "R", true},
		{"suspended", "0                 S", "S", true},
		// ST must not be shadowed by S
		{"stopped", "0                 ST", "ST", true},
		// Newly handled states
		{"timeout", "0:15              TO", "TO", true},
		{"oom", "0:9               OOM", "OOM", true},
		// Completely unknown state should not match
		{"unknown state XY", "0                 XY", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := re.FindString(tt.squeueLine)
			if tt.wantMatch {
				if match != tt.wantState {
					t.Errorf("regex matched %q, want %q for input %q", match, tt.wantState, tt.squeueLine)
				}
			} else {
				if match != "" {
					t.Errorf("regex unexpectedly matched %q for input %q", match, tt.squeueLine)
				}
			}
		})
	}
}

// TestExitCodeRegexForTimeout checks that the exit-code regex correctly extracts the
// signal number from the squeue output format "exit_code:signal  STATE".
// For a TIMEOUT job SLURM reports "0:15" meaning exit code 0 killed by signal 15 (SIGTERM).
func TestExitCodeRegexForTimeout(t *testing.T) {
	re := regexp.MustCompile(SlurmExitCodePattern)

	tests := []struct {
		name       string
		squeueLine string
		wantCode   string
	}{
		// TIMEOUT: squeue reports "0:15  TO" → the regex picks up "15" (the SIGTERM signal).
		{"timeout sigterm", "0:15              TO", "15"},
		// OOM: killed by signal 9.
		{"oom sigkill", "0:9               OOM", "9"},
		// Normal completion.
		{"success", "0                 CD", "0"},
		// Non-zero exit.
		{"failure", "1                 F", "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := re.FindStringSubmatch(tt.squeueLine)
			if len(matches) < 2 {
				t.Fatalf("exit-code regex did not match input %q", tt.squeueLine)
			}
			got := matches[1]
			if got != tt.wantCode {
				t.Errorf("exit code = %q, want %q for input %q", got, tt.wantCode, tt.squeueLine)
			}
		})
	}
}

// TestTerminatedContainerStatusTO verifies that terminatedContainerStatus, the helper
// used in the "TO" case of StatusHandler, produces a ContainerStatus with
// Reason=ReasonSlurmJobTimeout and Message=MessageSlurmJobTimeout.
func TestTerminatedContainerStatusTO(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	cs := terminatedContainerStatus("test", start, finish, 15, ReasonSlurmJobTimeout, MessageSlurmJobTimeout)

	if cs.Name != "test" {
		t.Errorf("Name = %q, want %q", cs.Name, "test")
	}
	if cs.Ready {
		t.Error("Ready should be false")
	}
	term := cs.State.Terminated
	if term == nil {
		t.Fatal("expected Terminated state, got nil")
	}
	if term.Reason != ReasonSlurmJobTimeout {
		t.Errorf("Reason = %q, want %q", term.Reason, ReasonSlurmJobTimeout)
	}
	if term.Message != MessageSlurmJobTimeout {
		t.Errorf("Message = %q, want %q", term.Message, MessageSlurmJobTimeout)
	}
	if term.ExitCode != 15 {
		t.Errorf("ExitCode = %d, want 15", term.ExitCode)
	}
	if !term.StartedAt.Time.Equal(start) {
		t.Errorf("StartedAt = %v, want %v", term.StartedAt.Time, start)
	}
	if !term.FinishedAt.Time.Equal(finish) {
		t.Errorf("FinishedAt = %v, want %v", term.FinishedAt.Time, finish)
	}
}

// TestTerminatedContainerStatusOOM verifies that terminatedContainerStatus, the helper
// used in the "OOM" case of StatusHandler, produces a ContainerStatus with
// Reason=ReasonOOMKilled (the Kubernetes convention) and Message=MessageOOMKilled.
func TestTerminatedContainerStatusOOM(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	cs := terminatedContainerStatus("mycontainer", start, finish, 9, ReasonOOMKilled, MessageOOMKilled)

	if cs.Name != "mycontainer" {
		t.Errorf("Name = %q, want %q", cs.Name, "mycontainer")
	}
	if cs.Ready {
		t.Error("Ready should be false")
	}
	term := cs.State.Terminated
	if term == nil {
		t.Fatal("expected Terminated state, got nil")
	}
	// "OOMKilled" is the Kubernetes convention used by the kubelet itself.
	if term.Reason != "OOMKilled" {
		t.Errorf("Reason = %q, want %q (Kubernetes convention)", term.Reason, "OOMKilled")
	}
	if term.Message != MessageOOMKilled {
		t.Errorf("Message = %q, want %q", term.Message, MessageOOMKilled)
	}
	if term.ExitCode != 9 {
		t.Errorf("ExitCode = %d, want 9", term.ExitCode)
	}
	if !term.StartedAt.Time.Equal(start) {
		t.Errorf("StartedAt = %v, want %v", term.StartedAt.Time, start)
	}
	if !term.FinishedAt.Time.Equal(finish) {
		t.Errorf("FinishedAt = %v, want %v", term.FinishedAt.Time, finish)
	}
}

// TestTerminatedContainerStatusNoReason verifies that terminatedContainerStatus works
// for ordinary termination cases (CD/F/ST/default) where no named reason is needed.
func TestTerminatedContainerStatusNoReason(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finish := time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)
	cs := terminatedContainerStatus("worker", start, finish, 0, "", "")

	term := cs.State.Terminated
	if term == nil {
		t.Fatal("expected Terminated state, got nil")
	}
	if term.Reason != "" {
		t.Errorf("Reason = %q, want empty string for ordinary termination", term.Reason)
	}
	if term.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", term.ExitCode)
	}
}

// TestStatusReadsComputeNodeFromJobWorkDir covers pods that run in the directory
// named by the slurm-job.vk.io/job-workdir annotation. job.slurm records the
// compute node in the job working directory, so the status has to read it from
// there and not from DataRootFolder, or NodeName is never reported for them.
func TestStatusReadsComputeNodeFromJobWorkDir(t *testing.T) {
	const (
		namespace = "default"
		podUID    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		node      = "node042.hpc.example.org"
	)

	t.Cleanup(resetStatusCache)

	squeue := filepath.Join(t.TempDir(), "squeue")
	if err := os.WriteFile(squeue, []byte("#!/bin/sh\necho '0:0 R'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		annotate bool // the pod carries slurm-job.vk.io/job-workdir
		cached   bool // the JIDs cache holds the job and its WorkDir
	}{
		{name: "default directory"},
		{name: "job-workdir annotation", annotate: true},
		{name: "job-workdir annotation and a running job", annotate: true, cached: true},
		{name: "work directory known only to the JIDs cache", cached: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataRoot := t.TempDir() + "/"
			base := t.TempDir()

			workDir := dataRoot + namespace + "-" + podUID
			var annotations map[string]string
			jids := map[string]*JidStruct{}
			if tt.annotate || tt.cached {
				workDir = filepath.Join(base, namespace+"-"+podUID)
			}
			if tt.annotate {
				annotations = map[string]string{"slurm-job.vk.io/job-workdir": base}
			}
			if tt.cached {
				jids[podUID] = &JidStruct{JID: "1", PodUID: podUID, PodNamespace: namespace, WorkDir: workDir}
			}

			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workDir, ComputeNodeFileName), []byte(node+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "cn-pod",
					Namespace:   namespace,
					UID:         podUID,
					Annotations: annotations,
				},
				Spec: v1.PodSpec{Containers: []v1.Container{{Name: "main"}}},
			}
			body, err := json.Marshal([]*v1.Pod{pod})
			if err != nil {
				t.Fatal(err)
			}

			h := &SidecarHandler{
				Config: SlurmConfig{Squeuepath: squeue, DataRootFolder: dataRoot},
				JIDs:   &jids,
				Ctx:    context.Background(),
			}
			resetStatusCache()
			rec := httptest.NewRecorder()
			h.StatusHandler(rec, httptest.NewRequest(http.MethodGet, "/status", bytes.NewReader(body)))

			if rec.Code != http.StatusOK {
				t.Fatalf("StatusHandler() code = %d, body %q", rec.Code, rec.Body.String())
			}
			var resp []commonIL.PodStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode status: %v (body %q)", err, rec.Body.String())
			}
			if len(resp) != 1 {
				t.Fatalf("got %d pod statuses, want 1", len(resp))
			}
			if resp[0].NodeName != node {
				t.Errorf("NodeName = %q, want %q (recorded in %s)", resp[0].NodeName, node, workDir)
			}
		})
	}
}

// The job creates the socket directory before it records the compute node, so the
// status reports the directory exactly when it reports a node.
func TestStatusReportsSocketDirWithNodeName(t *testing.T) {
	const node = "node007.hpc.example.org"

	t.Cleanup(resetStatusCache)

	squeue := filepath.Join(t.TempDir(), "squeue")
	if err := os.WriteFile(squeue, []byte("#!/bin/sh\necho '0:0 R'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		uid     string
		root    string
		started bool // the job has recorded its compute node
		want    string
	}{
		{name: "running job", uid: socketTestUID, root: socketTestRoot, started: true, want: socketTestDir},
		{name: "queued job", uid: socketTestUID, root: socketTestRoot},
		{name: "no SocketDirRoot", uid: socketTestUID, started: true},
		{name: "UID too short", uid: socketTestShortUID, root: socketTestRoot, started: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataRoot := t.TempDir() + "/"
			if tt.started {
				podDir := dataRoot + metav1.NamespaceDefault + "-" + tt.uid
				if err := os.MkdirAll(podDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(podDir, ComputeNodeFileName), []byte(node+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: socketTestPod, Namespace: metav1.NamespaceDefault, UID: types.UID(tt.uid)},
				Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "main"}}},
			}
			body, err := json.Marshal([]*v1.Pod{pod})
			if err != nil {
				t.Fatal(err)
			}

			jids := map[string]*JidStruct{}
			h := &SidecarHandler{
				Config: SlurmConfig{Squeuepath: squeue, DataRootFolder: dataRoot, SocketDirRoot: tt.root},
				JIDs:   &jids,
				Ctx:    context.Background(),
			}
			resetStatusCache()
			rec := httptest.NewRecorder()
			h.StatusHandler(rec, httptest.NewRequest(http.MethodGet, "/status", bytes.NewReader(body)))

			var resp []commonIL.PodStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp) != 1 {
				t.Fatalf("decode status: %v (code %d, body %q)", err, rec.Code, rec.Body.String())
			}
			if tt.started && resp[0].NodeName != node {
				t.Errorf("NodeName = %q, want %q", resp[0].NodeName, node)
			}
			if resp[0].SocketDir != tt.want {
				t.Errorf("SocketDir = %q, want %q", resp[0].SocketDir, tt.want)
			}
		})
	}
}

// TestStatusCacheIsPerPod reproduces two pods on one virtual kubelet, which asks
// about one pod per request: the second request, within the cache lifetime, must
// get its own pod's status, not the first pod's cached answer.
func TestStatusCacheIsPerPod(t *testing.T) {
	const podA, podB, ns = "uid-a", "uid-b", "default"
	t.Cleanup(resetStatusCache)
	resetStatusCache()

	squeue := filepath.Join(t.TempDir(), "squeue")
	if err := os.WriteFile(squeue, []byte("#!/bin/sh\necho '0:0 PD'\n"), 0o700); err != nil { //nolint:gosec // the fake squeue must be executable
		t.Fatal(err)
	}
	jids := map[string]*JidStruct{
		podA: {JID: "1", PodUID: podA, PodNamespace: ns},
		podB: {JID: "2", PodUID: podB, PodNamespace: ns},
	}
	h := &SidecarHandler{
		Config: SlurmConfig{Squeuepath: squeue, DataRootFolder: t.TempDir() + "/"},
		JIDs:   &jids,
		Ctx:    context.Background(),
	}
	ask := func(uid string) []commonIL.PodStatus {
		t.Helper()
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-" + uid, Namespace: ns, UID: types.UID(uid)},
			Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "main"}}},
		}
		body, err := json.Marshal([]*v1.Pod{pod})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.StatusHandler(rec, httptest.NewRequest(http.MethodGet, "/status", bytes.NewReader(body)))
		var resp []commonIL.PodStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode status: %v (body %q)", err, rec.Body.String())
		}
		return resp
	}

	if got := ask(podA); len(got) != 1 || got[0].PodUID != podA {
		t.Fatalf("first request: got %+v, want the status of uid-a", got)
	}
	got := ask(podB)
	if len(got) != 1 || got[0].PodUID != podB {
		t.Fatalf("second request within the cache lifetime: got %+v, want the status of uid-b", got)
	}
	// Asked again within the lifetime, uid-a is answered from the cache.
	if got := ask(podA); len(got) != 1 || got[0].PodUID != podA {
		t.Fatalf("cached request: got %+v, want the status of uid-a", got)
	}
}
