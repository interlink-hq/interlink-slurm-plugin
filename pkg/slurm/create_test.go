package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// A job script sent by the virtual kubelet is written to jobScript.sh in the job directory;
// the file must be closed, or a FUSE mount keeps it as .fuse_hidden* after the pod is deleted.
func TestSubmitHandlerClosesJobScript(t *testing.T) {
	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		t.Skip("needs /proc/self/fd")
	}
	sbatch := filepath.Join(t.TempDir(), "sbatch")
	if err := os.WriteFile(sbatch, []byte("#!/bin/sh\necho 'Submitted batch job 42'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir() + "/"
	data := commonIL.RetrievedPodData{
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: socketTestPod, Namespace: metav1.NamespaceDefault, UID: socketTestUID},
			Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "workload", Image: busyboxImage}}},
		},
		JobScript: "#!/bin/sh\necho hello\n",
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
		},
		JIDs: &jids,
		Ctx:  context.Background(),
	}
	rec := httptest.NewRecorder()
	h.SubmitHandler(rec, httptest.NewRequest(http.MethodPost, "/create", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("SubmitHandler() code = %d, body %q", rec.Code, rec.Body.String())
	}

	workDir := dataRoot + metav1.NamespaceDefault + "-" + socketTestUID
	if raw, err := os.ReadFile(workDir + "/jobScript.sh"); err != nil || string(raw) != data.JobScript {
		t.Fatalf("jobScript.sh = %q, %v; want %q", raw, err, data.JobScript)
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && strings.HasPrefix(target, workDir+"/") {
			t.Errorf("left %s open (fd %s)", target, fd.Name())
		}
	}
}

// A pod whose containers set no CPU or memory limit gets its flavor's CPUDefault and
// MemoryDefault in job.slurm, not the 1 CPU / 1 MB fallback. Pod limits and the flags
// annotation rank above the flavor, as the README documents. An init container's limit
// alone does not lower the job below the flavor default: the app containers it precedes
// have no limit.
func TestSubmitHandlerFlavorDefaults(t *testing.T) {
	limits := func(cpu, mem string) v1.ResourceRequirements {
		return v1.ResourceRequirements{Limits: v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(mem)}}
	}
	for _, tc := range []struct {
		name      string
		init      []v1.Container
		app       v1.ResourceRequirements
		flags     string
		cpus, mem string
	}{
		{name: "no limits", cpus: "4", mem: "2048"},
		{name: "init container limits only", init: []v1.Container{{Name: "setup", Image: busyboxImage, Resources: limits("1", "100Mi")}}, cpus: "4", mem: "2048"},
		{name: "init container limits above the flavor", init: []v1.Container{{Name: "setup", Image: busyboxImage, Resources: limits("6", "3Gi")}}, cpus: "6", mem: "3072"},
		{name: "annotation flags", flags: "--cpus-per-task=8 --mem=8192", cpus: "8", mem: "8192"},
		{name: "init container limits and annotation flags", init: []v1.Container{{Name: "setup", Image: busyboxImage, Resources: limits("16", "16Gi")}}, flags: "--cpus-per-task=8 --mem=8192", cpus: "16", mem: "16384"},
		{name: "app limits, init container without", init: []v1.Container{{Name: "setup", Image: busyboxImage}}, app: limits("2", "1Gi"), cpus: "2", mem: "1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sbatch := filepath.Join(t.TempDir(), "sbatch")
			if err := os.WriteFile(sbatch, []byte("#!/bin/sh\necho 'Submitted batch job 42'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			dataRoot := t.TempDir() + "/"
			data := commonIL.RetrievedPodData{
				Pod: v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: socketTestPod, Namespace: metav1.NamespaceDefault, UID: socketTestUID,
						Annotations: map[string]string{"slurm-job.vk.io/flags": tc.flags},
					},
					Spec: v1.PodSpec{
						InitContainers: tc.init,
						Containers:     []v1.Container{{Name: "workload", Image: busyboxImage, Resources: tc.app}},
					},
				},
				JobScript: "#!/bin/sh\necho hello\n",
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
					Flavors:          map[string]FlavorConfig{"small": {Name: "small", CPUDefault: 4, MemoryDefault: "2G"}},
					DefaultFlavor:    "small",
				},
				JIDs: &jids,
				Ctx:  context.Background(),
			}
			rec := httptest.NewRecorder()
			h.SubmitHandler(rec, httptest.NewRequest(http.MethodPost, "/create", bytes.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("SubmitHandler() code = %d, body %q", rec.Code, rec.Body.String())
			}

			raw, err := os.ReadFile(dataRoot + metav1.NamespaceDefault + "-" + socketTestUID + "/job.slurm")
			if err != nil {
				t.Fatal(err)
			}
			// every --cpus-per-task and --mem line carries the expected value
			for flag, want := range map[string]string{"--cpus-per-task": tc.cpus, "--mem": tc.mem} {
				var got []string
				for _, line := range strings.Split(string(raw), "\n") {
					if v, ok := strings.CutPrefix(line, "#SBATCH "+flag+"="); ok {
						got = append(got, v)
					}
				}
				if len(got) == 0 || slices.ContainsFunc(got, func(v string) bool { return v != want }) {
					t.Errorf("%s = %q, want %s:\n%s", flag, got, want, raw)
				}
			}
		})
	}
}
