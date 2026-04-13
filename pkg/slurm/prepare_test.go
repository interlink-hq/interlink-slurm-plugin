package slurm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStringToHex(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "test",
			expected: "74657374",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "string with spaces",
			input:    "a b",
			expected: "6162",
		},
		{
			name:     "special characters",
			input:    "a-b_c",
			expected: "612d625f63",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stringToHex(tt.input)
			if result != tt.expected {
				t.Errorf("stringToHex(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParsingTimeFromString(t *testing.T) {
	ctx := context.Background()
	timestampFormat := "2006-01-02 15:04:05.999999999 -0700 MST"

	tests := []struct {
		name        string
		input       string
		shouldError bool
	}{
		{
			name:        "valid timestamp",
			input:       "2024-01-15 10:30:45.123456789 +0000 UTC",
			shouldError: false,
		},
		{
			name:        "invalid format - missing fields",
			input:       "2024-01-15 10:30:45",
			shouldError: true,
		},
		{
			name:        "invalid format - wrong separator",
			input:       "2024-01-15T10:30:45.123456789+0000UTC",
			shouldError: true,
		},
		{
			name:        "empty string",
			input:       "",
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parsingTimeFromString(ctx, tt.input, timestampFormat)
			if tt.shouldError {
				if err == nil {
					t.Errorf("parsingTimeFromString(%q) expected error but got nil", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("parsingTimeFromString(%q) unexpected error: %v", tt.input, err)
				}
				if result.IsZero() {
					t.Errorf("parsingTimeFromString(%q) returned zero time", tt.input)
				}
			}
		})
	}
}

func TestPrepareImage(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name             string
		config           SlurmConfig
		metadata         metav1.ObjectMeta
		containerImage   string
		expectedContains string
	}{
		{
			name: "image with default prefix",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "ubuntu:latest",
			expectedContains: "docker://ubuntu:latest",
		},
		{
			name: "image with custom prefix from annotation",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata: metav1.ObjectMeta{
				Annotations: map[string]string{
					"slurm-job.vk.io/image-root": "oras://",
				},
			},
			containerImage:   "myimage:v1",
			expectedContains: "oras://myimage:v1",
		},
		{
			name: "absolute path image",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "/path/to/image.sif",
			expectedContains: "/path/to/image.sif",
		},
		{
			name: "image already has prefix",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "docker://nginx:alpine",
			expectedContains: "docker://nginx:alpine",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := prepareImage(ctx, tt.config, tt.metadata, tt.containerImage)
			if result != tt.expectedContains {
				t.Errorf("prepareImage() = %q, want %q", result, tt.expectedContains)
			}
		})
	}
}

func TestProduceSLURMScriptSupportsShortAnnotationFlags(t *testing.T) {
	ctx := context.Background()
	workingDir := t.TempDir()

	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "helloworld-bubble-pod",
			Namespace: "default",
			UID:       "bca0ba6d-b9cb-499e-a16f-700f61a1b030",
			Annotations: map[string]string{
				"slurm-job.vk.io/flags": "--job-name=helloworld-pod -A geant4 -p geant4",
			},
		},
	}

	config := SlurmConfig{
		BashPath: "/bin/bash",
	}

	resourceLimits := ResourceLimits{
		CPU:    12,
		Memory: 12 * 1024 * 1024 * 1024,
	}

	_, err := produceSLURMScript(ctx, config, pod, workingDir, pod.ObjectMeta, nil, resourceLimits, false, false, nil)
	if err != nil {
		t.Fatalf("produceSLURMScript() unexpected error: %v", err)
	}

	jobSlurm, err := os.ReadFile(filepath.Join(workingDir, "job.slurm"))
	if err != nil {
		t.Fatalf("failed to read generated job.slurm: %v", err)
	}

	content := string(jobSlurm)
	expectedLines := []string{
		"#SBATCH --job-name=bca0ba6d-b9cb-499e-a16f-700f61a1b030",
		"#SBATCH --job-name=helloworld-pod",
		"#SBATCH -A geant4",
		"#SBATCH -p geant4",
		"#SBATCH --cpus-per-task=12",
		"#SBATCH --mem=12288",
	}

	for _, expectedLine := range expectedLines {
		if !strings.Contains(content, expectedLine) {
			t.Errorf("generated job.slurm missing line %q\ncontent:\n%s", expectedLine, content)
		}
	}

	unexpectedLines := []string{
		"#SBATCH -A\n",
		"#SBATCH -p\n",
		"\n#SBATCH geant4\n",
	}

	for _, unexpectedLine := range unexpectedLines {
		if strings.Contains(content, unexpectedLine) {
			t.Errorf("generated job.slurm contains malformed directive %q\ncontent:\n%s", unexpectedLine, content)
		}
	}
}

func TestCheckIfJidExists(t *testing.T) {
	ctx := context.Background()
	jids := make(map[string]*JidStruct)

	// Add some test data
	jids["uid-1"] = &JidStruct{
		PodUID:       "uid-1",
		PodNamespace: "default",
		JID:          "12345",
		StartTime:    time.Now(),
	}

	tests := []struct {
		name     string
		uid      string
		expected bool
	}{
		{
			name:     "existing JID",
			uid:      "uid-1",
			expected: true,
		},
		{
			name:     "non-existing JID",
			uid:      "uid-2",
			expected: false,
		},
		{
			name:     "empty uid",
			uid:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checkIfJidExists(ctx, &jids, tt.uid)
			if result != tt.expected {
				t.Errorf("checkIfJidExists(%q) = %v, want %v", tt.uid, result, tt.expected)
			}
		})
	}
}

func TestPrepareMountsPersistentVolume(t *testing.T) {
	ctx := context.Background()
	prefix = ""

	workingPath := t.TempDir()
	container := v1.Container{
		Name: "app",
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "shared-data",
				MountPath: "/data",
			},
		},
	}

	podData := commonIL.RetrievedPodData{
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nfs-pod",
				UID:  "pod-uid-123",
				Annotations: map[string]string{
					"slurm-job.vk.io/nfs-mount-options": "vers=4.1,hard",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{container},
				Volumes: []v1.Volume{
					{
						Name: "shared-data",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: "shared-data-pvc",
							},
						},
					},
				},
			},
		},
		Containers: []commonIL.RetrievedContainer{
			{
				Name: "app",
				PersistentVolumeClaims: []v1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pvc"},
						Spec: v1.PersistentVolumeClaimSpec{
							VolumeName: "shared-data-pv",
						},
					},
				},
				PersistentVolumes: []v1.PersistentVolume{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pv"},
						Spec: v1.PersistentVolumeSpec{
							MountOptions: []string{"nolock"},
							PersistentVolumeSource: v1.PersistentVolumeSource{
								NFS: &v1.NFSVolumeSource{
									Server: "10.0.0.15",
									Path:   "/exports/shared-data",
								},
							},
						},
					},
				},
			},
		},
	}

	mounts, err := prepareMounts(ctx, SlurmConfig{ContainerRuntime: "singularity"}, &podData, &container, workingPath)
	if err != nil {
		t.Fatalf("prepareMounts() unexpected error: %v", err)
	}

	expectedHostPath := filepath.Join("/tmp", "interlink-nfs", "pod-uid-123", "shared-data")
	if !strings.Contains(mounts, "--bind "+expectedHostPath+":/data:rw") {
		t.Fatalf("prepareMounts() = %q, want bind mount for %q", mounts, expectedHostPath)
	}

	if !strings.Contains(prefix, "mount -t nfs") {
		t.Fatalf("prefix %q does not contain NFS mount command", prefix)
	}
	if !strings.Contains(prefix, "10.0.0.15:/exports/shared-data") {
		t.Fatalf("prefix %q does not contain NFS source", prefix)
	}
	if !strings.Contains(prefix, "nolock,vers=4.1,hard,rw") {
		t.Fatalf("prefix %q does not contain merged NFS options", prefix)
	}
	if !strings.Contains(prefix, "interlink_cleanup_nfs()") {
		t.Fatalf("prefix %q does not register NFS cleanup trap", prefix)
	}
}

func TestPrepareMountsPersistentVolumeFUSE(t *testing.T) {
	ctx := context.Background()
	prefix = ""

	workingPath := t.TempDir()
	container := v1.Container{
		Name: "app",
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "shared-data",
				MountPath: "/data",
				SubPath:   "nested",
				ReadOnly:  true,
			},
		},
	}

	podData := commonIL.RetrievedPodData{
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nfs-pod",
				UID:  "pod-uid-123",
				Annotations: map[string]string{
					"slurm-job.vk.io/nfs-mode":           "fuse",
					"slurm-job.vk.io/nfs-fusemount-type": "container-daemon",
					"slurm-job.vk.io/nfs-fuse-command":   "/usr/local/bin/fuse-nfs {{SOURCE}} -o {{OPTIONS}}",
					"slurm-job.vk.io/nfs-mount-options":  "vers=4.1,hard",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{container},
				Volumes: []v1.Volume{
					{
						Name: "shared-data",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: "shared-data-pvc",
							},
						},
					},
				},
			},
		},
		Containers: []commonIL.RetrievedContainer{
			{
				Name: "app",
				PersistentVolumeClaims: []v1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pvc"},
						Spec: v1.PersistentVolumeClaimSpec{
							VolumeName: "shared-data-pv",
						},
					},
				},
				PersistentVolumes: []v1.PersistentVolume{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pv"},
						Spec: v1.PersistentVolumeSpec{
							PersistentVolumeSource: v1.PersistentVolumeSource{
								NFS: &v1.NFSVolumeSource{
									Server: "10.0.0.15",
									Path:   "/exports/shared-data",
								},
							},
						},
					},
				},
			},
		},
	}

	mounts, err := prepareMounts(ctx, SlurmConfig{ContainerRuntime: "singularity"}, &podData, &container, workingPath)
	if err != nil {
		t.Fatalf("prepareMounts() unexpected error: %v", err)
	}

	if !strings.Contains(mounts, "--fusemount") {
		t.Fatalf("prepareMounts() = %q, want fuse mount", mounts)
	}
	if !strings.Contains(mounts, "container-daemon:/usr/local/bin/fuse-nfs") {
		t.Fatalf("prepareMounts() = %q, want fuse mount type and command", mounts)
	}
	if !strings.Contains(mounts, "/exports/shared-data/nested") {
		t.Fatalf("prepareMounts() = %q, want subPath appended to NFS path", mounts)
	}
	if !strings.Contains(mounts, "/data") {
		t.Fatalf("prepareMounts() = %q, want container mount path", mounts)
	}
	if !strings.Contains(mounts, "vers=4.1,hard,ro") {
		t.Fatalf("prepareMounts() = %q, want merged NFS options", mounts)
	}
	if strings.Contains(prefix, "mount -t nfs") {
		t.Fatalf("prefix %q should not contain host NFS mount commands in fuse mode", prefix)
	}
}

func TestPrepareMountsPersistentVolumeHelper(t *testing.T) {
	ctx := context.Background()
	prefix = ""

	workingPath := t.TempDir()
	container := v1.Container{
		Name: "app",
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "shared-data",
				MountPath: "/data",
				SubPath:   "nested",
			},
		},
	}

	podData := commonIL.RetrievedPodData{
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nfs-pod",
				UID:  "pod-uid-123",
				Annotations: map[string]string{
					"slurm-job.vk.io/nfs-mode":                  "helper",
					"slurm-job.vk.io/nfs-helper-command":        "/opt/interlink/bin/pvc-live-helper --server {{SERVER}} --path {{PATH}} --source {{SOURCE}} --options {{OPTIONS}} --readonly {{READONLY}} --pvc {{PVC_NAME}} --pv {{PV_NAME}}",
					"slurm-job.vk.io/nfs-helper-fusemount-type": "host-daemon",
					"slurm-job.vk.io/nfs-mount-options":         "vers=4.1,hard",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{container},
				Volumes: []v1.Volume{
					{
						Name: "shared-data",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: "shared-data-pvc",
							},
						},
					},
				},
			},
		},
		Containers: []commonIL.RetrievedContainer{
			{
				Name: "app",
				PersistentVolumeClaims: []v1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pvc"},
						Spec: v1.PersistentVolumeClaimSpec{
							VolumeName: "shared-data-pv",
						},
					},
				},
				PersistentVolumes: []v1.PersistentVolume{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pv"},
						Spec: v1.PersistentVolumeSpec{
							PersistentVolumeSource: v1.PersistentVolumeSource{
								NFS: &v1.NFSVolumeSource{
									Server: "10.0.0.15",
									Path:   "/exports/shared-data",
								},
							},
						},
					},
				},
			},
		},
	}

	mounts, err := prepareMounts(ctx, SlurmConfig{ContainerRuntime: "singularity"}, &podData, &container, workingPath)
	if err != nil {
		t.Fatalf("prepareMounts() unexpected error: %v", err)
	}

	helperScriptPath := filepath.Join(workingPath, "interlink-nfs-live-helper-shared-data.sh")
	if !strings.Contains(mounts, "--fusemount") {
		t.Fatalf("prepareMounts() = %q, want helper fuse mount", mounts)
	}
	if !strings.Contains(mounts, "host-daemon:"+helperScriptPath) {
		t.Fatalf("prepareMounts() = %q, want helper script path %q", mounts, helperScriptPath)
	}
	if strings.Contains(prefix, "mount -t nfs") {
		t.Fatalf("prefix %q should not contain host NFS mount commands in helper mode", prefix)
	}

	helperScript, err := os.ReadFile(helperScriptPath)
	if err != nil {
		t.Fatalf("could not read helper script %s: %v", helperScriptPath, err)
	}

	helperScriptContents := string(helperScript)
	if !strings.Contains(helperScriptContents, "/opt/interlink/bin/pvc-live-helper") {
		t.Fatalf("helper script %q does not contain helper command", helperScriptContents)
	}
	if !strings.Contains(helperScriptContents, "--path /exports/shared-data/nested") {
		t.Fatalf("helper script %q does not contain rendered subPath", helperScriptContents)
	}
	if !strings.Contains(helperScriptContents, "--options vers=4.1,hard,rw") {
		t.Fatalf("helper script %q does not contain rendered mount options", helperScriptContents)
	}
	if !strings.Contains(helperScriptContents, `"$mountpoint"`) {
		t.Fatalf("helper script %q does not append mountpoint", helperScriptContents)
	}
}

func TestPrepareMountsPersistentVolumeBridge(t *testing.T) {
	ctx := context.Background()
	prefix = ""

	workingPath := t.TempDir()
	container := v1.Container{
		Name: "app",
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "shared-data",
				MountPath: "/data",
				SubPath:   "nested",
				ReadOnly:  true,
			},
		},
	}

	podData := commonIL.RetrievedPodData{
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nfs-pod",
				UID:  "pod-uid-123",
				Annotations: map[string]string{
					"slurm-job.vk.io/nfs-mode":        "bridge",
					"slurm-job.vk.io/nfs-bridge-pvc":  "shared-data-pvc",
					"slurm-job.vk.io/nfs-bridge-path": "/tmp/interlink-pvc-bridge/pod-uid-123/shared-data-pvc",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{container},
				Volumes: []v1.Volume{
					{
						Name: "shared-data",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: "shared-data-pvc",
							},
						},
					},
				},
			},
		},
		Containers: []commonIL.RetrievedContainer{
			{
				Name: "app",
				PersistentVolumeClaims: []v1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pvc"},
						Spec: v1.PersistentVolumeClaimSpec{
							VolumeName: "shared-data-pv",
						},
					},
				},
				PersistentVolumes: []v1.PersistentVolume{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "shared-data-pv"},
						Spec: v1.PersistentVolumeSpec{
							PersistentVolumeSource: v1.PersistentVolumeSource{
								NFS: &v1.NFSVolumeSource{
									Server: "10.0.0.15",
									Path:   "/exports/shared-data",
								},
							},
						},
					},
				},
			},
		},
	}

	mounts, err := prepareMounts(ctx, SlurmConfig{ContainerRuntime: "singularity"}, &podData, &container, workingPath)
	if err != nil {
		t.Fatalf("prepareMounts() unexpected error: %v", err)
	}

	expectedBridgePath := "/tmp/interlink-pvc-bridge/pod-uid-123/shared-data-pvc/nested"
	if !strings.Contains(mounts, "--bind "+expectedBridgePath+":/data:ro") {
		t.Fatalf("prepareMounts() = %q, want bridged bind mount for %q", mounts, expectedBridgePath)
	}
	if strings.Contains(prefix, "mount -t nfs") {
		t.Fatalf("prefix %q should not contain host NFS mount commands in bridge mode", prefix)
	}
}

func TestRemoveJID(t *testing.T) {
	jids := make(map[string]*JidStruct)
	jids["uid-1"] = &JidStruct{
		PodUID:       "uid-1",
		PodNamespace: "default",
		JID:          "12345",
	}
	jids["uid-2"] = &JidStruct{
		PodUID:       "uid-2",
		PodNamespace: "default",
		JID:          "67890",
	}

	removeJID("uid-1", &jids)

	if _, exists := jids["uid-1"]; exists {
		t.Error("removeJID() failed to remove uid-1")
	}

	if _, exists := jids["uid-2"]; !exists {
		t.Error("removeJID() incorrectly removed uid-2")
	}
}
