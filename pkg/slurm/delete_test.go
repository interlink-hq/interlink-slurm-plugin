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

// /delete of a gang member still in the buffer drops it: the group forgets it, its
// directory goes, nothing is cancelled, and the next members form the group afresh.
func TestStopHandlerDropsBufferedGangMember(t *testing.T) {
	h, dataRoot, scancelCount, sbatchCount := newPodGroupHandler(t, "echo 'Submitted batch job 42'")
	first := kueueMemberPod("uid-g0", "group", 0, 2)
	rec := submitPod(t, h, first)
	var created CreateStruct
	if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusOK || err != nil || created.PodJID != "" {
		t.Fatalf("buffered member: %d %q", rec.Code, rec.Body.String())
	}

	if err := deletePod(h, first); err != nil {
		t.Fatal(err)
	}
	if e, ok := h.GangTable[gangKey(testNamespace, "group")]; ok {
		t.Fatalf("the group still holds the deleted member: %+v", e.Members)
	}
	if _, err := os.Stat(memberDir(dataRoot, "uid-g0")); !os.IsNotExist(err) {
		t.Fatalf("the deleted member's directory is left: %v", err)
	}
	if n := scancelCalls(t, scancelCount); n != 0 {
		t.Fatalf("scancel ran %d times for a member that had no job", n)
	}

	// The members sent again make one job of two, not one of three.
	for i, uid := range []string{"uid-h0", "uid-h1"} {
		if rec := submitPod(t, h, kueueMemberPod(uid, "group", i, 2)); rec.Code != http.StatusOK {
			t.Fatalf("member %s: %d %q", uid, rec.Code, rec.Body.String())
		}
	}
	if n := countLines(t, sbatchCount); n != 1 {
		t.Fatalf("sbatch ran %d times, want once", n)
	}
	if n := len(h.GangTable[gangKey(testNamespace, "group")].Members); n != 2 {
		t.Fatalf("the job holds %d members, want 2", n)
	}
}
