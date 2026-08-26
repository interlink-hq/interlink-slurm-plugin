package slurm

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		{
			name: "oras image not double-prefixed with docker",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "oras://myregistry.example.com/myimage:v1",
			expectedContains: "oras://myregistry.example.com/myimage:v1",
		},
		{
			name: "library image not double-prefixed",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "library://user/collection/image:tag",
			expectedContains: "library://user/collection/image:tag",
		},
		{
			name: "shub image not double-prefixed",
			config: SlurmConfig{
				ImagePrefix: "docker://",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "shub://vsoch/hello-world",
			expectedContains: "shub://vsoch/hello-world",
		},
		{
			name: "empty prefix with plain image",
			config: SlurmConfig{
				ImagePrefix: "",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "busybox:1.35",
			expectedContains: "busybox:1.35",
		},
		{
			name: "empty prefix with oras image",
			config: SlurmConfig{
				ImagePrefix: "",
			},
			metadata:         metav1.ObjectMeta{},
			containerImage:   "oras://myregistry.example.com/myimage:latest",
			expectedContains: "oras://myregistry.example.com/myimage:latest",
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

func TestGetJobWorkDir(t *testing.T) {
	config := SlurmConfig{
		DataRootFolder: "/default/root/",
	}
	namespace := "mynamespace"
	podUID := "abc-123"
	defaultPath := config.DataRootFolder + namespace + "-" + podUID

	tests := []struct {
		name        string
		annotations map[string]string
		expected    string
	}{
		{
			name:        "no annotation uses default",
			annotations: map[string]string{},
			expected:    defaultPath,
		},
		{
			name:        "annotation overrides base dir",
			annotations: map[string]string{"slurm-job.vk.io/job-workdir": "/scratch/mygroup"},
			expected:    "/scratch/mygroup/" + namespace + "-" + podUID,
		},
		{
			name:        "annotation with trailing slash",
			annotations: map[string]string{"slurm-job.vk.io/job-workdir": "/scratch/mygroup/"},
			expected:    "/scratch/mygroup/" + namespace + "-" + podUID,
		},
		{
			name:        "empty annotation value uses default",
			annotations: map[string]string{"slurm-job.vk.io/job-workdir": ""},
			expected:    defaultPath,
		},
		{
			name:        "relative path annotation is rejected, uses default",
			annotations: map[string]string{"slurm-job.vk.io/job-workdir": "relative/path"},
			expected:    defaultPath,
		},
		{
			name:        "path traversal annotation is rejected, uses default",
			annotations: map[string]string{"slurm-job.vk.io/job-workdir": "/scratch/../etc"},
			expected:    defaultPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getJobWorkDir(config, tt.annotations, namespace, podUID)
			if result != tt.expected {
				t.Errorf("getJobWorkDir() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestPrepareMountsSimpleVolumeProjectedHeredoc verifies that when SHARED_FS is
// not set (non-shared filesystem mode), multiline projected volume data (e.g. a
// PEM certificate from kube-root-ca.crt) is written using a base64-encoded
// heredoc in the generated SLURM script prefix, so that newlines are preserved
// when SLURM exports environment variables to compute nodes.
func TestPrepareMountsSimpleVolumeProjectedHeredoc(t *testing.T) {
	ctx := context.Background()
	workingDir := t.TempDir()

	// Ensure SHARED_FS is unset so the non-shared-fs code path is exercised.
	t.Setenv("SHARED_FS", "false")

	multilineCert := "-----BEGIN CERTIFICATE-----\n" +
		"MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n" +
		"test\n" +
		"-----END CERTIFICATE-----\n"

	defaultMode := int32(0644)
	projectedVolume := v1.Volume{
		Name: "kube-api-access",
		VolumeSource: v1.VolumeSource{
			Projected: &v1.ProjectedVolumeSource{
				DefaultMode: &defaultMode,
				Sources:     []v1.VolumeProjection{},
			},
		},
	}

	volumeMount := v1.VolumeMount{
		Name:      "kube-api-access",
		MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
	}

	container := &v1.Container{
		Name: "mycontainer",
		VolumeMounts: []v1.VolumeMount{
			volumeMount,
		},
	}

	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-api-access",
		},
		Data: map[string]string{
			"ca.crt": multilineCert,
		},
	}

	config := SlurmConfig{
		ExportPodData: true,
	}

	// Reset the global prefix before the test.
	prefix = ""

	var mountedDataSB strings.Builder
	err := prepareMountsSimpleVolume(ctx, config, container, workingDir, configMap, volumeMount, projectedVolume, &mountedDataSB)
	if err != nil {
		t.Fatalf("prepareMountsSimpleVolume() unexpected error: %v", err)
	}

	// The generated prefix must use a base64-decoded heredoc (base64 -d <<'MARKER')
	// rather than echo "${VAR}", so that newlines inside the certificate are preserved.
	if !strings.Contains(prefix, "base64 -d <<'") {
		t.Errorf("prefix does not contain base64 heredoc (base64 -d <<'): prefix = %q", prefix)
	}
	if strings.Contains(prefix, "echo \"${") {
		t.Errorf("prefix must not use echo to write file content: prefix = %q", prefix)
	}

	// The mkdir -p command must use an absolute path (starting with "/") so that
	// the parent directory is created at the correct location on the SLURM compute
	// node. A relative path would create the directory relative to the SLURM job's
	// working directory, not at the absolute path used by the subsequent heredoc.
	if !strings.Contains(prefix, "mkdir -p \"/") {
		t.Errorf("prefix mkdir -p must use an absolute path (got relative): prefix = %q", prefix)
	}

	// Extract the base64 content from between "base64 -d <<'MARKER'\n" and "\nMARKER".
	// This is more robust than scanning for lines that look like base64.
	const heredocCmdPrefix = "base64 -d <<'"
	cmdIdx := strings.Index(prefix, heredocCmdPrefix)
	if cmdIdx == -1 {
		t.Fatalf("could not find heredoc command in prefix: %q", prefix)
	}
	// Find end of the "base64 -d <<'MARKER'" line to get the marker name.
	markerStart := cmdIdx + len(heredocCmdPrefix)
	markerEnd := strings.Index(prefix[markerStart:], "'")
	if markerEnd == -1 {
		t.Fatalf("could not find closing quote for heredoc marker in prefix: %q", prefix)
	}
	marker := prefix[markerStart : markerStart+markerEnd]

	// The heredoc content is between the first newline after the command line and the
	// closing marker on its own line.
	contentStart := markerStart + markerEnd + 1 // skip closing quote
	newlineAfterCmd := strings.Index(prefix[contentStart:], "\n")
	if newlineAfterCmd == -1 {
		t.Fatalf("could not find newline after heredoc command in prefix: %q", prefix)
	}
	contentStart += newlineAfterCmd + 1
	markerLine := "\n" + marker
	contentEnd := strings.Index(prefix[contentStart:], markerLine)
	if contentEnd == -1 {
		t.Fatalf("could not find closing heredoc marker %q in prefix: %q", marker, prefix)
	}
	b64Content := prefix[contentStart : contentStart+contentEnd]

	decoded, err := base64.StdEncoding.DecodeString(b64Content)
	if err != nil {
		t.Fatalf("failed to decode base64 content %q: %v", b64Content, err)
	}
	if string(decoded) != multilineCert {
		t.Errorf("decoded content = %q, want %q", string(decoded), multilineCert)
	}

	// The prefix must end with exactly the heredoc end-marker and nothing else
	// on that line. produceSLURMScript appends "\n" + f.Name() after the prefix,
	// so if the prefix ended with "VKDATA_abc /path/to/job.sh" bash would not
	// recognise the end-of-heredoc and would consume job.sh into the heredoc.
	if !strings.HasSuffix(prefix, "\n"+marker) {
		t.Errorf("prefix must end with \"\\n%s\" so the heredoc terminator is on its own line; got suffix %q",
			marker, prefix[max(0, len(prefix)-len(marker)-20):])
	}
}

// TestPrepareMountsSimpleVolumeProjectedSharedFS verifies that when SHARED_FS=true,
// multiline projected volume data (e.g. a PEM certificate from kube-root-ca.crt) is
// written directly to the shared filesystem via os.WriteFile, preserving newlines
// exactly, and that no heredoc is added to the SLURM script prefix.
func TestPrepareMountsSimpleVolumeProjectedSharedFS(t *testing.T) {
	ctx := context.Background()
	workingDir := t.TempDir()

	t.Setenv("SHARED_FS", "true")

	multilineCert := "-----BEGIN CERTIFICATE-----\n" +
		"MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\n" +
		"test\n" +
		"-----END CERTIFICATE-----\n"

	defaultMode := int32(0644)
	projectedVolume := v1.Volume{
		Name: "kube-api-access",
		VolumeSource: v1.VolumeSource{
			Projected: &v1.ProjectedVolumeSource{
				DefaultMode: &defaultMode,
				Sources:     []v1.VolumeProjection{},
			},
		},
	}

	volumeMount := v1.VolumeMount{
		Name:      "kube-api-access",
		MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
	}

	container := &v1.Container{
		Name: "mycontainer",
		VolumeMounts: []v1.VolumeMount{
			volumeMount,
		},
	}

	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-api-access",
		},
		Data: map[string]string{
			"ca.crt": multilineCert,
		},
	}

	config := SlurmConfig{
		ExportPodData: true,
	}

	// Reset the global prefix before the test.
	prefix = ""

	var mountedDataSB strings.Builder
	err := prepareMountsSimpleVolume(ctx, config, container, workingDir, configMap, volumeMount, projectedVolume, &mountedDataSB)
	if err != nil {
		t.Fatalf("prepareMountsSimpleVolume() unexpected error: %v", err)
	}

	// With SHARED_FS=true the plugin writes files directly; no heredoc should be
	// added to the SLURM script prefix.
	if strings.Contains(prefix, "base64 -d <<'") {
		t.Errorf("prefix must not contain base64 heredoc with SHARED_FS=true: prefix = %q", prefix)
	}

	// The file must exist on the shared filesystem with byte-for-byte correct content.
	expectedFilePath := filepath.Join(workingDir, "projectedVolumeMaps", volumeMount.Name, "ca.crt")
	gotBytes, err := os.ReadFile(expectedFilePath)
	if err != nil {
		t.Fatalf("os.WriteFile did not create file %s: %v", expectedFilePath, err)
	}
	if string(gotBytes) != multilineCert {
		t.Errorf("file content = %q, want %q", string(gotBytes), multilineCert)
	}

	// The bind mount path must be included in the mounts string.
	mounts := mountedDataSB.String()
	if !strings.Contains(mounts, expectedFilePath) {
		t.Errorf("mountedDataSB does not contain expected host path %q: got %q", expectedFilePath, mounts)
	}
	containerMountPath := filepath.Join(volumeMount.MountPath, "ca.crt")
	if !strings.Contains(mounts, containerMountPath) {
		t.Errorf("mountedDataSB does not contain expected container path %q: got %q", containerMountPath, mounts)
	}
}

// TestNormalizeVolumeFileContent verifies that normalizeVolumeFileContent properly
// handles the common misconfiguration where a PEM certificate (or any multiline
// value) is stored in the VK YAML config without a block scalar (|), causing the
// YAML parser to deliver literal \n sequences instead of real newlines.
func TestNormalizeVolumeFileContent(t *testing.T) {
const pemWithRealNewlines = "-----BEGIN CERTIFICATE-----\nMIIFakeCert==\n-----END CERTIFICATE-----\n"
const pemWithLiteralBackslashN = `-----BEGIN CERTIFICATE-----\nMIIFakeCert==\n-----END CERTIFICATE-----\n`

tests := []struct {
name  string
input string
want  string
}{
{
name:  "already has real newlines - no change",
input: pemWithRealNewlines,
want:  pemWithRealNewlines,
},
{
name:  "literal backslash-n only - unescape to real newlines",
input: pemWithLiteralBackslashN,
want:  pemWithRealNewlines,
},
{
name:  "plain text without any newlines or escape sequences - no change",
input: "hello world",
want:  "hello world",
},
{
name:  "mixed real newlines and literal backslash-n - no change (real newlines present)",
input: "line1\nli\\ne2\nline3\n",
want:  "line1\nli\\ne2\nline3\n",
},
{
name:  "empty string - no change",
input: "",
want:  "",
},
}

for _, tc := range tests {
t.Run(tc.name, func(t *testing.T) {
got := normalizeVolumeFileContent(tc.input)
if string(got) != tc.want {
t.Errorf("normalizeVolumeFileContent(%q) = %q, want %q", tc.input, got, tc.want)
}
})
}
}

// TestDeleteContainerWithoutJID covers deleting a pod that never got a Slurm job:
// sbatch may have been rejected, or the plugin may have restarted since submission.
// Both leave no JIDs entry, and reading the JID unguarded panicked the handler.
func TestDeleteContainerWithoutJID(t *testing.T) {
	dir := t.TempDir()
	podDir := filepath.Join(dir, "ns-11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(podDir, 0o755); err != nil {
		t.Fatalf("failed to create pod dir: %v", err)
	}

	JIDs := map[string]*JidStruct{}
	err := deleteContainer(context.Background(), SlurmConfig{}, "11111111-2222-3333-4444-555555555555", &JIDs, podDir)
	if err != nil {
		t.Fatalf("deleteContainer returned an error for a pod with no job: %v", err)
	}
	if _, statErr := os.Stat(podDir); !os.IsNotExist(statErr) {
		t.Errorf("expected the pod directory to be removed, stat returned %v", statErr)
	}
}

// mesh.sh unshares a network namespace, sets the mesh up inside it and then execs
// its "$@". If job.sh is emitted on the following line instead of as that argument,
// mesh.sh exits first and the workload runs outside the namespace: the job succeeds
// but has no mesh connectivity, which is silent and hard to diagnose.
func TestProduceSLURMScriptRunsWorkloadInsideMeshNetns(t *testing.T) {
	ctx := context.Background()
	workingDir := t.TempDir()

	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mesh-pod",
			Namespace: "default",
			UID:       "11111111-2222-3333-4444-555555555555",
			Annotations: map[string]string{
				"slurm-job.vk.io/pre-exec": "cat <<'EOFMESH' > $TMPDIR/mesh.sh\n#!/bin/bash\nexec \"$@\"\nEOFMESH\n",
			},
		},
	}

	if _, err := produceSLURMScript(ctx, SlurmConfig{BashPath: "/bin/bash"}, pod, workingDir, pod.ObjectMeta, nil, ResourceLimits{CPU: 1, Memory: 1024 * 1024}, false, false, nil); err != nil {
		t.Fatalf("produceSLURMScript() unexpected error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(workingDir, "job.slurm"))
	if err != nil {
		t.Fatalf("read job.slurm: %v", err)
	}

	want := filepath.Join(workingDir, "mesh.sh") + " " + filepath.Join(workingDir, "job.sh")
	if !strings.Contains(string(raw), want) {
		t.Errorf("job.sh must be passed to mesh.sh as its argument (%q)\n---\n%s", want, string(raw))
	}
}

// Without mesh.sh the separator must stay a newline: with SHARED_FS=false the
// prefix ends in a base64 heredoc end-marker, and gluing job.sh onto that line
// makes bash swallow the rest of the script instead of ending the heredoc.
func TestProduceSLURMScriptKeepsNewlineBeforeJobScriptWithoutMesh(t *testing.T) {
	ctx := context.Background()
	workingDir := t.TempDir()

	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plain-pod",
			Namespace: "default",
			UID:       "66666666-7777-8888-9999-000000000000",
		},
	}

	if _, err := produceSLURMScript(ctx, SlurmConfig{BashPath: "/bin/bash"}, pod, workingDir, pod.ObjectMeta, nil, ResourceLimits{CPU: 1, Memory: 1024 * 1024}, false, false, nil); err != nil {
		t.Fatalf("produceSLURMScript() unexpected error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(workingDir, "job.slurm"))
	if err != nil {
		t.Fatalf("read job.slurm: %v", err)
	}

	jobScript := filepath.Join(workingDir, "job.sh")
	if !strings.Contains(string(raw), "\n"+jobScript) {
		t.Errorf("job.sh must start its own line when no mesh script is in play\n---\n%s", string(raw))
	}
}
