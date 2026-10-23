package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
