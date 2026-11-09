package slurm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "k8s.io/api/core/v1"

	"github.com/intertwin-eu/interlink-slurm-plugin/pkg/batchsystem"
)

func TestSlurmBatchSystemIsRegisteredWithDefaultFactory(t *testing.T) {
	bs, err := batchsystem.CreateBatchSystem(context.Background(), batchsystem.FactoryConfig{
		Type:   batchsystem.BatchSystemSLURM,
		Config: SlurmConfig{DataRootFolder: t.TempDir() + "/"},
	})
	if err != nil {
		t.Fatalf("CreateBatchSystem(slurm) error = %v", err)
	}
	if _, ok := bs.(*SlurmBatchSystem); !ok {
		t.Fatalf("CreateBatchSystem(slurm) returned %T, want *SlurmBatchSystem", bs)
	}
}

func TestNewSlurmBatchSystemConfigTypes(t *testing.T) {
	cfg := SlurmConfig{DataRootFolder: t.TempDir() + "/"}

	for _, tc := range []struct {
		name    string
		config  interface{}
		wantErr bool
	}{
		{name: "value", config: cfg},
		{name: "pointer", config: &cfg},
		{name: "nil pointer", config: (*SlurmConfig)(nil), wantErr: true},
		{name: "wrong type", config: "not a config", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs, err := NewSlurmBatchSystem(context.Background(), tc.config)
			if tc.wantErr {
				if err == nil {
					t.Fatal("NewSlurmBatchSystem() succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSlurmBatchSystem() error = %v", err)
			}
			got := bs.(*SlurmBatchSystem).Handler().Config.DataRootFolder
			if got != cfg.DataRootFolder {
				t.Errorf("handler DataRootFolder = %q, want %q", got, cfg.DataRootFolder)
			}
		})
	}
}

func TestSlurmBatchSystemInitializeCreatesDataRoot(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "nested", "root") + "/"
	bs, err := NewSlurmBatchSystem(context.Background(), SlurmConfig{DataRootFolder: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if st, err := os.Stat(dataRoot); err != nil || !st.IsDir() {
		t.Fatalf("DataRootFolder %s not created: stat err=%v", dataRoot, err)
	}
}

func TestSlurmBatchSystemGetLogs(t *testing.T) {
	const (
		namespace = "default"
		podUID    = "11111111-2222-3333-4444-555555555555"
	)
	dataRoot := t.TempDir() + "/"
	workDir := dataRoot + namespace + "-" + podUID
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("job.out", "job line\n")
	mustWrite("run-main.out", "l1\nl2\nl3\n")
	mustWrite("init-setup.out", "init output\n")

	bs, err := NewSlurmBatchSystem(context.Background(), SlurmConfig{DataRootFolder: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: namespace, UID: podUID}}

	t.Run("full output prepends job.out", func(t *testing.T) {
		out, err := bs.GetLogs(context.Background(), pod, "main", batchsystem.LogOptions{})
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		if want := "job line\nl1\nl2\nl3\n"; string(out) != want {
			t.Errorf("GetLogs() = %q, want %q", out, want)
		}
	})

	t.Run("tail", func(t *testing.T) {
		out, err := bs.GetLogs(context.Background(), pod, "main", batchsystem.LogOptions{Tail: 1})
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		if !strings.HasSuffix(string(out), "l3\n\n") || strings.Contains(string(out), "job line") {
			t.Errorf("GetLogs(Tail=1) = %q, want only the trailing lines", out)
		}
	})

	t.Run("falls back to init container output", func(t *testing.T) {
		out, err := bs.GetLogs(context.Background(), pod, "setup", batchsystem.LogOptions{})
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		if want := "job line\ninit output\n"; string(out) != want {
			t.Errorf("GetLogs() = %q, want %q", out, want)
		}
	})

	t.Run("missing container yields job output only", func(t *testing.T) {
		out, err := bs.GetLogs(context.Background(), pod, "nope", batchsystem.LogOptions{})
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		if want := "job line\n"; string(out) != want {
			t.Errorf("GetLogs() = %q, want %q", out, want)
		}
	})

	t.Run("nil pod", func(t *testing.T) {
		if _, err := bs.GetLogs(context.Background(), nil, "main", batchsystem.LogOptions{}); err == nil {
			t.Fatal("GetLogs(nil) succeeded, want error")
		}
	})
}

func TestSlurmBatchSystemStatusAndDelete(t *testing.T) {
	const (
		namespace = "default"
		podUID    = "aaaaaaaa-0000-1111-2222-333333333333"
	)
	t.Cleanup(resetStatusCache)
	resetStatusCache()

	bin := t.TempDir()
	squeue := filepath.Join(bin, "squeue")
	if err := os.WriteFile(squeue, []byte("#!/bin/sh\necho '0:0 R'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	scancel := filepath.Join(bin, "scancel")
	if err := os.WriteFile(scancel, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	dataRoot := t.TempDir() + "/"
	workDir := dataRoot + namespace + "-" + podUID
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bs, err := NewSlurmBatchSystem(context.Background(), SlurmConfig{
		DataRootFolder: dataRoot,
		Squeuepath:     squeue,
		Scancelpath:    scancel,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := bs.(*SlurmBatchSystem).Handler()
	(*h.JIDs)[podUID] = &JidStruct{JID: "42", PodUID: podUID, PodNamespace: namespace, WorkDir: workDir}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: namespace, UID: podUID},
		Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "main"}}},
	}

	statuses, err := bs.Status(context.Background(), []*v1.Pod{pod})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].PodUID != podUID {
		t.Fatalf("Status() = %+v, want one status for %s", statuses, podUID)
	}
	if len(statuses[0].Containers) != 1 || statuses[0].Containers[0].State.Running == nil {
		t.Errorf("Status() container state = %+v, want Running", statuses[0].Containers)
	}

	if err := bs.Delete(context.Background(), pod); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, ok := (*h.JIDs)[podUID]; ok {
		t.Error("Delete() left the job in the JIDs cache")
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("Delete() left the work directory %s (stat: %v)", workDir, err)
	}

	if err := bs.Delete(context.Background(), nil); err == nil {
		t.Error("Delete(nil) succeeded, want error")
	}
}
