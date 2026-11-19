package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
)

// SubmitHandler is what binds the socket directory into every container, outside
// prepareMounts and the env file; job.sh is where the runtime commands end up.
func TestSubmitHandlerBindsSocketDir(t *testing.T) {
	sbatch := filepath.Join(t.TempDir(), "sbatch")
	if err := os.WriteFile(sbatch, []byte("#!/bin/sh\necho 'Submitted batch job 42'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		root string
		want string // empty: job.sh must not mention INTERLINK_SOCKET_DIR
	}{
		{
			name: "with a root",
			root: socketTestRoot,
			want: "--bind " + socketTestDir + ":" + socketTestDir + " --env INTERLINK_SOCKET_DIR=" + socketTestDir + " " + busyboxImage,
		},
		{name: "without a root"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataRoot := t.TempDir() + "/"
			data := commonIL.RetrievedPodData{
				Pod: v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: socketTestPod, Namespace: metav1.NamespaceDefault, UID: socketTestUID},
					Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "workload", Image: busyboxImage}}},
				},
			}
			body, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}

			jids := map[string]*JidStruct{}
			h := &SidecarHandler{
				Config: SlurmConfig{
					Sbatchpath:       sbatch,
					ContainerRuntime: singularityRuntime,
					SingularityPath:  singularityRuntime,
					DataRootFolder:   dataRoot,
					SocketDirRoot:    tt.root,
				},
				JIDs: &jids,
				Ctx:  context.Background(),
			}
			rec := httptest.NewRecorder()
			h.SubmitHandler(rec, httptest.NewRequest(http.MethodPost, "/create", bytes.NewReader(body)))

			if rec.Code != http.StatusOK {
				t.Fatalf("SubmitHandler() code = %d, body %q", rec.Code, rec.Body.String())
			}
			raw, err := os.ReadFile(dataRoot + metav1.NamespaceDefault + "-" + socketTestUID + "/job.sh")
			if err != nil {
				t.Fatalf("read job.sh: %v", err)
			}
			jobSh := string(raw)
			if tt.want == "" {
				if strings.Contains(jobSh, "INTERLINK_SOCKET_DIR") {
					t.Errorf("job.sh must not bind a socket directory\n---\n%s", jobSh)
				}
				return
			}
			if !strings.Contains(jobSh, tt.want) {
				t.Errorf("job.sh lacks %q\n---\n%s", tt.want, jobSh)
			}
		})
	}
}

// /create answers the last member of a group as /createPodGroup answers a group:
// 503 for what trying again later can fix and 400 for a job Slurm refuses, with
// sbatch's text. The member leaves the group with its directory; the members
// before it stay buffered.
func TestSubmitHandlerAnswersGroupBySbatchRefusal(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		rc     int
		want   int
	}{
		{"ssh failed", "ssh: connect to host login.hpc-a.example.org port 22: Connection timed out", 255, http.StatusServiceUnavailable},
		{"QOS submit limit", "sbatch: error: QOSMaxSubmitJobPerUserLimit\nsbatch: error: Batch job submission failed: Job violates accounting/QOS policy (job submit limit, user's size and/or time limits)", 1, http.StatusServiceUnavailable},
		{"invalid account", "sbatch: error: Batch job submission failed: Invalid account or account/partition combination specified", 1, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(tt.stderr+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			firstLine, _, _ := strings.Cut(tt.stderr, "\n")
			h, dataRoot, _, _ := newPodGroupHandler(t, "cat "+filepath.Join(dir, "stderr")+" >&2; exit "+strconv.Itoa(tt.rc))

			var created CreateStruct
			rec := submitPod(t, h, kueueMemberPod("uid-g0", "group", 0, 2))
			if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusOK || err != nil || created.PodJID != "" {
				t.Fatalf("first member: %d %q", rec.Code, rec.Body.String())
			}
			rec = submitPod(t, h, kueueMemberPod("uid-g1", "group", 1, 2))
			if rec.Code != tt.want || !strings.Contains(rec.Body.String(), firstLine) {
				t.Fatalf("last member: %d %q, want %d with sbatch's text", rec.Code, rec.Body.String(), tt.want)
			}
			if _, err := os.Stat(memberDir(dataRoot, "uid-g1")); !os.IsNotExist(err) {
				t.Fatalf("the refused member's directory is left: %v", err)
			}
			if e := h.GangTable[gangKey(testNamespace, "group")]; e == nil || len(e.Members) != 1 || e.Members["uid-g0"] == nil {
				t.Fatalf("gang entry %+v, want uid-g0 still buffered", e)
			}
		})
	}
}

// A pod that names its group without the group's size is refused before anything
// is written; with gang scheduling off the same pod is one job.
func TestSubmitHandlerRefusesGroupWithoutSize(t *testing.T) {
	h, dataRoot, _, sbatchCount := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
	pod := kueueMemberPod("uid-g0", "group", 0, 2)
	delete(pod.Annotations, PodGroupTotalCountAnnotation)

	if rec := submitPod(t, h, pod); rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d %q, want 400", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(memberDir(dataRoot, "uid-g0")); !os.IsNotExist(err) {
		t.Fatalf("a refused pod left its directory: %v", err)
	}
	if n := countLines(t, sbatchCount); n != 0 || len(h.GangTable) != 0 {
		t.Fatalf("sbatch ran %d times, gangs %v", n, h.GangTable)
	}

	h.Config.GangSchedulingEnabled = false
	rec := submitPod(t, h, pod)
	var created CreateStruct
	if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusOK || err != nil || created.PodJID != "42" {
		t.Fatalf("gang scheduling off: %d %q, want job 42", rec.Code, rec.Body.String())
	}
}
