package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Delete removes the socket directory the job's EXIT trap left behind, for example
// after a SIGKILL. It recomputes the path from the UID, and a directory that is
// already gone is not an error.
func TestStopHandlerRemovesSocketDir(t *testing.T) {
	for _, exists := range []bool{true, false} {
		root := t.TempDir()
		socketDir := socketDirFor(root, socketTestUID)
		if exists {
			if err := os.MkdirAll(socketDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(socketDir, "fwd-8888.sock"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: socketTestPod, Namespace: metav1.NamespaceDefault, UID: socketTestUID}}
		body, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}

		jids := map[string]*JidStruct{}
		h := &SidecarHandler{
			Config: SlurmConfig{DataRootFolder: t.TempDir() + "/", SocketDirRoot: root},
			JIDs:   &jids,
			Ctx:    context.Background(),
		}
		rec := httptest.NewRecorder()
		h.StopHandler(rec, httptest.NewRequest(http.MethodPost, "/delete", bytes.NewReader(body)))

		if rec.Code != http.StatusOK {
			t.Errorf("StopHandler() with the directory present=%v: code %d, body %q", exists, rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(socketDir); !os.IsNotExist(err) {
			t.Errorf("socket directory %s still there after delete (stat: %v)", socketDir, err)
		}
	}
}
