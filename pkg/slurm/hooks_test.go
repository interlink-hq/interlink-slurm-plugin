package slurm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ---------------------------------------------------------------------------
// translateLifecycleHook
// ---------------------------------------------------------------------------

func TestTranslatePreStopHook_Nil(t *testing.T) {
	if got := translateLifecycleHook(nil); got != nil {
		t.Errorf("translateLifecycleHook(nil) = %v, want nil", got)
	}
}

func TestTranslatePreStopHook_ExecEmpty(t *testing.T) {
	handler := &v1.LifecycleHandler{
		Exec: &v1.ExecAction{Command: []string{}},
	}
	// Empty command slice should fall through and return nil (unsupported)
	if got := translateLifecycleHook(handler); got != nil {
		t.Errorf("translateLifecycleHook(exec with empty command) = %v, want nil", got)
	}
}

func TestTranslatePreStopHook_Exec(t *testing.T) {
	cmd := []string{"/bin/sh", "-c", "echo prestop"}
	handler := &v1.LifecycleHandler{
		Exec: &v1.ExecAction{Command: cmd},
	}
	got := translateLifecycleHook(handler)
	if got == nil {
		t.Fatal("translateLifecycleHook returned nil, expected LifecycleHookSpec")
	}
	if got.Type != LifecycleHookTypeExec {
		t.Errorf("Type = %q, want %q", got.Type, LifecycleHookTypeExec)
	}
	if len(got.ExecCommand) != len(cmd) {
		t.Errorf("ExecCommand len = %d, want %d", len(got.ExecCommand), len(cmd))
	}
	for i, v := range cmd {
		if got.ExecCommand[i] != v {
			t.Errorf("ExecCommand[%d] = %q, want %q", i, got.ExecCommand[i], v)
		}
	}
}

func TestTranslatePreStopHook_HTTPGet_Defaults(t *testing.T) {
	handler := &v1.LifecycleHandler{
		HTTPGet: &v1.HTTPGetAction{
			Port: intstr.FromInt(8080),
			// Scheme, Host, Path intentionally empty → should be filled with defaults
		},
	}
	got := translateLifecycleHook(handler)
	if got == nil {
		t.Fatal("translateLifecycleHook returned nil, expected LifecycleHookSpec")
	}
	if got.Type != LifecycleHookTypeHTTPGet {
		t.Errorf("Type = %q, want %q", got.Type, LifecycleHookTypeHTTPGet)
	}
	if got.HTTPGet.Scheme != "http" {
		t.Errorf("Scheme = %q, want %q", got.HTTPGet.Scheme, "http")
	}
	if got.HTTPGet.Host != "localhost" {
		t.Errorf("Host = %q, want %q", got.HTTPGet.Host, "localhost")
	}
	if got.HTTPGet.Path != "/" {
		t.Errorf("Path = %q, want %q", got.HTTPGet.Path, "/")
	}
	if got.HTTPGet.Port != 8080 {
		t.Errorf("Port = %d, want 8080", got.HTTPGet.Port)
	}
}

func TestTranslatePreStopHook_HTTPGet_Explicit(t *testing.T) {
	handler := &v1.LifecycleHandler{
		HTTPGet: &v1.HTTPGetAction{
			Scheme: "HTTPS",
			Host:   "myhost",
			Port:   intstr.FromInt(9090),
			Path:   "/shutdown",
		},
	}
	got := translateLifecycleHook(handler)
	if got == nil {
		t.Fatal("translateLifecycleHook returned nil, expected LifecycleHookSpec")
	}
	if got.HTTPGet.Scheme != "https" {
		t.Errorf("Scheme = %q, want %q", got.HTTPGet.Scheme, "https")
	}
	if got.HTTPGet.Host != "myhost" {
		t.Errorf("Host = %q, want myhost", got.HTTPGet.Host)
	}
	if got.HTTPGet.Port != 9090 {
		t.Errorf("Port = %d, want 9090", got.HTTPGet.Port)
	}
	if got.HTTPGet.Path != "/shutdown" {
		t.Errorf("Path = %q, want /shutdown", got.HTTPGet.Path)
	}
}

func TestTranslatePreStopHook_HTTPGet_NamedPort(t *testing.T) {
	handler := &v1.LifecycleHandler{
		HTTPGet: &v1.HTTPGetAction{
			Port: intstr.FromString("http"),
		},
	}
	// Named ports cannot be resolved; should be skipped (returns nil)
	if got := translateLifecycleHook(handler); got != nil {
		t.Errorf("translateLifecycleHook(named port) = %v, want nil", got)
	}
}

func TestTranslatePreStopHook_NoExecNoHTTPGet(t *testing.T) {
	handler := &v1.LifecycleHandler{} // neither exec nor httpGet
	if got := translateLifecycleHook(handler); got != nil {
		t.Errorf("translateLifecycleHook(empty handler) = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// generatePreStopTrap
// ---------------------------------------------------------------------------

// testSlurmConfig returns a minimal SlurmConfig suitable for unit-testing
// generatePreStopTrap.  It uses a recognisable singularity path so tests can
// assert that exec hooks are dispatched via the container runtime.
func testSlurmConfig() SlurmConfig {
	return SlurmConfig{
		SingularityPath:           "/usr/bin/singularity",
		SingularityDefaultOptions: []string{"--nv"},
		ImagePrefix:               "docker://",
	}
}

func TestGeneratePreStopTrap_NoHooks(t *testing.T) {
	commands := []ContainerCommand{
		{containerName: "app", isInitContainer: false, preStopHook: nil},
	}
	if got := generatePreStopTrap(testSlurmConfig(), commands); got != "" {
		t.Errorf("generatePreStopTrap with no hooks = %q, want empty string", got)
	}
}

func TestGeneratePreStopTrap_InitContainerIgnored(t *testing.T) {
	commands := []ContainerCommand{
		{
			containerName:   "init",
			isInitContainer: true,
			preStopHook: &LifecycleHookSpec{
				Type:        LifecycleHookTypeExec,
				ExecCommand: []string{"echo", "init"},
			},
		},
	}
	if got := generatePreStopTrap(testSlurmConfig(), commands); got != "" {
		t.Errorf("generatePreStopTrap should ignore init containers, got %q", got)
	}
}

func TestGeneratePreStopTrap_ExecHook_WithRuntime(t *testing.T) {
	commands := []ContainerCommand{
		{
			containerName:   "app",
			isInitContainer: false,
			containerImage:  "docker://ubuntu:latest",
			preStopHook: &LifecycleHookSpec{
				Type:        LifecycleHookTypeExec,
				ExecCommand: []string{"/bin/sh", "-c", "echo 'goodbye'"},
			},
		},
	}
	got := generatePreStopTrap(testSlurmConfig(), commands)
	if got == "" {
		t.Fatal("generatePreStopTrap returned empty string, expected script fragment")
	}
	// Must define the trap function and install it
	if !strings.Contains(got, "preStopTrap()") {
		t.Error("expected 'preStopTrap()' function definition in output")
	}
	if !strings.Contains(got, "trap preStopTrap SIGTERM") {
		t.Error("expected 'trap preStopTrap SIGTERM' in output")
	}
	// Exec hook must be dispatched via singularity exec (coherent with executeExecProbe)
	if !strings.Contains(got, "/usr/bin/singularity") {
		t.Error("expected singularity path in exec hook invocation")
	}
	if !strings.Contains(got, "exec") {
		t.Error("expected 'exec' subcommand in singularity invocation")
	}
	if !strings.Contains(got, socketDirExecArgs) {
		t.Error("expected the socket directory bind in the exec hook invocation")
	}
	// Exec hook must include a timeout
	if !strings.Contains(got, "timeout") {
		t.Error("expected 'timeout' in exec hook invocation")
	}
	// The exec command arguments should be present (shell-escaped)
	if !strings.Contains(got, "/bin/sh") {
		t.Error("expected exec command '/bin/sh' in output")
	}
	// Output should be redirected to the container-specific log file
	if !strings.Contains(got, "prestop-app.out") {
		t.Error("expected 'prestop-app.out' in output")
	}
	// Must forward SIGTERM to running containers
	if !strings.Contains(got, `kill "${pid}"`) {
		t.Error("expected kill command for running containers in output")
	}
}

func TestGeneratePreStopTrap_ExecHook_NoRuntime(t *testing.T) {
	// When singularity is not configured the exec hook falls back to host execution.
	cfg := SlurmConfig{} // SingularityPath is empty
	commands := []ContainerCommand{
		{
			containerName:   "app",
			isInitContainer: false,
			preStopHook: &LifecycleHookSpec{
				Type:        LifecycleHookTypeExec,
				ExecCommand: []string{"/bin/sh", "-c", "echo 'goodbye'"},
			},
		},
	}
	got := generatePreStopTrap(cfg, commands)
	if got == "" {
		t.Fatal("generatePreStopTrap returned empty string, expected script fragment")
	}
	if !strings.Contains(got, "timeout 30") {
		t.Error("expected host-side 'timeout 30' in fallback exec hook invocation")
	}
	if !strings.Contains(got, "/bin/sh") {
		t.Error("expected exec command '/bin/sh' in output")
	}
	// Must NOT contain a singularity invocation in the fallback path
	if strings.Contains(got, "singularity") {
		t.Error("fallback exec hook should not contain 'singularity'")
	}
}

func TestGeneratePreStopTrap_HTTPGetHook(t *testing.T) {
	commands := []ContainerCommand{
		{
			containerName:   "sidecar",
			isInitContainer: false,
			preStopHook: &LifecycleHookSpec{
				Type: LifecycleHookTypeHTTPGet,
				HTTPGet: &LifecycleHTTPGetSpec{
					Scheme: "http",
					Host:   "localhost",
					Port:   8080,
					Path:   "/stop",
				},
			},
		},
	}
	got := generatePreStopTrap(testSlurmConfig(), commands)
	if got == "" {
		t.Fatal("generatePreStopTrap returned empty string, expected script fragment")
	}
	if !strings.Contains(got, "curl") {
		t.Error("expected curl invocation for httpGet hook")
	}
	if !strings.Contains(got, "http://localhost:8080/stop") {
		t.Error("expected URL 'http://localhost:8080/stop' in output")
	}
	if !strings.Contains(got, "prestop-sidecar.out") {
		t.Error("expected 'prestop-sidecar.out' in output")
	}
}

func TestGeneratePreStopTrap_MultipleContainers(t *testing.T) {
	commands := []ContainerCommand{
		{
			containerName:   "app",
			isInitContainer: false,
			containerImage:  "docker://ubuntu:latest",
			preStopHook: &LifecycleHookSpec{
				Type:        LifecycleHookTypeExec,
				ExecCommand: []string{"echo", "bye-app"},
			},
		},
		{
			containerName:   "sidecar",
			isInitContainer: false,
			preStopHook: &LifecycleHookSpec{
				Type: LifecycleHookTypeHTTPGet,
				HTTPGet: &LifecycleHTTPGetSpec{
					Scheme: "http",
					Host:   "localhost",
					Port:   9000,
					Path:   "/",
				},
			},
		},
		{
			containerName:   "no-hook",
			isInitContainer: false,
			preStopHook:     nil,
		},
	}
	got := generatePreStopTrap(testSlurmConfig(), commands)
	if !strings.Contains(got, "prestop-app.out") {
		t.Error("expected hook output for 'app' container")
	}
	if !strings.Contains(got, "prestop-sidecar.out") {
		t.Error("expected hook output for 'sidecar' container")
	}
	if strings.Contains(got, "no-hook") {
		t.Error("container with no hook should not appear in trap script")
	}
	// Only one trap installation
	if strings.Count(got, "trap preStopTrap SIGTERM") != 1 {
		t.Error("expected exactly one 'trap preStopTrap SIGTERM' statement")
	}
}

// ---------------------------------------------------------------------------
// generatePostStartScript
// ---------------------------------------------------------------------------

func TestGeneratePostStartScript_NoHook(t *testing.T) {
	cmd := ContainerCommand{containerName: "app", isInitContainer: false, postStartHook: nil}
	if got := generatePostStartScript(testSlurmConfig(), cmd); got != "" {
		t.Errorf("generatePostStartScript with no hook = %q, want empty string", got)
	}
}

func TestGeneratePostStartScript_InitContainerIgnored(t *testing.T) {
	cmd := ContainerCommand{
		containerName:   "init",
		isInitContainer: true,
		postStartHook: &LifecycleHookSpec{
			Type:        LifecycleHookTypeExec,
			ExecCommand: []string{"echo", "init"},
		},
	}
	if got := generatePostStartScript(testSlurmConfig(), cmd); got != "" {
		t.Errorf("generatePostStartScript should ignore init containers, got %q", got)
	}
}

func TestGeneratePostStartScript_ExecHook_WithRuntime(t *testing.T) {
	cmd := ContainerCommand{
		containerName:   "app",
		isInitContainer: false,
		containerImage:  "docker://python:3.11-alpine",
		postStartHook: &LifecycleHookSpec{
			Type:        LifecycleHookTypeExec,
			ExecCommand: []string{"/bin/sh", "-c", "echo hello > /tmp/marker"},
		},
	}
	got := generatePostStartScript(testSlurmConfig(), cmd)
	if got == "" {
		t.Fatal("generatePostStartScript returned empty string, expected script fragment")
	}
	// Must use singularity exec (coherent with executeExecProbe)
	if !strings.Contains(got, "/usr/bin/singularity") {
		t.Error("expected singularity path in exec hook invocation")
	}
	if !strings.Contains(got, "timeout") {
		t.Error("expected 'timeout' in exec hook invocation")
	}
	if !strings.Contains(got, socketDirExecArgs) {
		t.Error("expected the socket directory bind in the postStart exec hook")
	}
	if !strings.Contains(got, "/bin/sh") {
		t.Error("expected exec command '/bin/sh' in output")
	}
	// Output appended to container's run log, not a separate prestop file
	if !strings.Contains(got, "run-app.out") {
		t.Error("expected 'run-app.out' as output destination")
	}
}

func TestGeneratePostStartScript_ExecHook_NoRuntime(t *testing.T) {
	cfg := SlurmConfig{} // SingularityPath is empty
	cmd := ContainerCommand{
		containerName:   "app",
		isInitContainer: false,
		postStartHook: &LifecycleHookSpec{
			Type:        LifecycleHookTypeExec,
			ExecCommand: []string{"/bin/sh", "-c", "echo hello"},
		},
	}
	got := generatePostStartScript(cfg, cmd)
	if got == "" {
		t.Fatal("generatePostStartScript returned empty string")
	}
	if !strings.Contains(got, "timeout 30") {
		t.Error("expected host-side 'timeout 30' in fallback exec hook")
	}
	if strings.Contains(got, "singularity") {
		t.Error("fallback should not contain 'singularity'")
	}
}

func TestGeneratePostStartScript_HTTPGetHook(t *testing.T) {
	cmd := ContainerCommand{
		containerName:   "sidecar",
		isInitContainer: false,
		postStartHook: &LifecycleHookSpec{
			Type: LifecycleHookTypeHTTPGet,
			HTTPGet: &LifecycleHTTPGetSpec{
				Scheme: "http",
				Host:   "localhost",
				Port:   8080,
				Path:   "/init",
			},
		},
	}
	got := generatePostStartScript(testSlurmConfig(), cmd)
	if got == "" {
		t.Fatal("generatePostStartScript returned empty string")
	}
	if !strings.Contains(got, "curl") {
		t.Error("expected curl for httpGet hook")
	}
	if !strings.Contains(got, "http://localhost:8080/init") {
		t.Error("expected URL in output")
	}
	if !strings.Contains(got, "run-sidecar.out") {
		t.Error("expected 'run-sidecar.out' as output destination")
	}
}

// ---------------------------------------------------------------------------
// findTmpBindHostPath
// ---------------------------------------------------------------------------

func TestFindTmpBindHostPath_Empty(t *testing.T) {
	if got := findTmpBindHostPath(nil); got != "" {
		t.Errorf("findTmpBindHostPath(nil) = %q, want empty", got)
	}
	if got := findTmpBindHostPath([]string{}); got != "" {
		t.Errorf("findTmpBindHostPath([]) = %q, want empty", got)
	}
}

func TestFindTmpBindHostPath_NoTmpMount(t *testing.T) {
	// In the real runtime command the volume bind-mounts are combined into a
	// single string element by prepareMounts (see Create.go).  We test that
	// layout here.
	runtimeCmd := []string{
		"singularity", "exec", "--containall",
		"--bind /host/path1:/var/run/secrets:ro --bind /host/path2:/data",
		"docker://python:3.11-alpine",
	}
	if got := findTmpBindHostPath(runtimeCmd); got != "" {
		t.Errorf("findTmpBindHostPath = %q, want empty when /tmp not bound", got)
	}
}

func TestFindTmpBindHostPath_WithTmpMount(t *testing.T) {
	// Single combined mounts element — the real layout from prepareMounts.
	runtimeCmd := []string{
		"singularity", "exec", "--containall",
		"--bind /host/data:/data:ro --bind /host/emptydir:/tmp",
		"docker://python:3.11-alpine",
	}
	got := findTmpBindHostPath(runtimeCmd)
	if got != "/host/emptydir" {
		t.Errorf("findTmpBindHostPath = %q, want %q", got, "/host/emptydir")
	}
}

func TestFindTmpBindHostPath_WithTmpMount_SeparateElements(t *testing.T) {
	// Also verify the function works when bind arguments are separate slice
	// elements (future-proofing / defence in depth).
	runtimeCmd := []string{
		"singularity", "exec", "--containall",
		"--bind", "/host/emptydir:/tmp",
		"docker://python:3.11-alpine",
	}
	got := findTmpBindHostPath(runtimeCmd)
	if got != "/host/emptydir" {
		t.Errorf("findTmpBindHostPath (separate elements) = %q, want %q", got, "/host/emptydir")
	}
}

func TestFindTmpBindHostPath_TmpWithOptions(t *testing.T) {
	runtimeCmd := []string{
		"singularity", "exec", "--containall",
		"--bind /host/emptydir:/tmp:rw",
		"docker://python:3.11-alpine",
	}
	got := findTmpBindHostPath(runtimeCmd)
	if got != "/host/emptydir" {
		t.Errorf("findTmpBindHostPath = %q, want %q", got, "/host/emptydir")
	}
}

func TestFindTmpBindHostPath_TmpSubpathNotMatched(t *testing.T) {
	// /tmp-data and /tmp/sub should NOT match — only exact /tmp
	runtimeCmd := []string{
		"singularity", "exec",
		"--bind /host/path:/tmp-data --bind /host/path2:/tmp/sub",
		"docker://python:3.11-alpine",
	}
	if got := findTmpBindHostPath(runtimeCmd); got != "" {
		t.Errorf("findTmpBindHostPath = %q; sub-paths and suffixed names should not match", got)
	}
}

// ---------------------------------------------------------------------------
// injectTmpBindMount — /tmp already bound
// ---------------------------------------------------------------------------

func TestInjectTmpBindMount_SkipsWhenTmpAlreadyBound(t *testing.T) {
	runtimeCmd := []string{
		"singularity", "exec", "--containall",
		"--bind /host/emptydir:/tmp",
		"docker://python:3.11-alpine",
	}
	got := injectTmpBindMount(runtimeCmd, singularityRuntime)
	// Should return the original slice unchanged
	if len(got) != len(runtimeCmd) {
		t.Errorf("injectTmpBindMount should not inject when /tmp already bound: got len %d, want %d", len(got), len(runtimeCmd))
	}
	joined := strings.Join(got, " ")
	if strings.Count(joined, ":/tmp") != 1 {
		t.Errorf("expected exactly one :/tmp bind spec, got: %s", joined)
	}
}

// ---------------------------------------------------------------------------
// generatePostStartScript — /tmp already bound
// ---------------------------------------------------------------------------

func TestGeneratePostStartScript_UseExistingTmpMount(t *testing.T) {
	// Simulate a container whose runtimeCommand already has /tmp bound to a volume.
	cmd := ContainerCommand{
		containerName:  "app",
		containerImage: "docker://python:3.11-alpine",
		runtimeCommand: []string{
			"singularity", "exec", "--containall",
			"--bind /host/emptydir:/tmp",
			"docker://python:3.11-alpine",
		},
		postStartHook: &LifecycleHookSpec{
			Type:        LifecycleHookTypeExec,
			ExecCommand: []string{"/bin/sh", "-c", "echo hook > /tmp/marker"},
		},
	}
	got := generatePostStartScript(testSlurmConfig(), cmd)
	if got == "" {
		t.Fatal("generatePostStartScript returned empty string")
	}
	// Should use the existing host path, not hook-tmp
	if !strings.Contains(got, "/host/emptydir:/tmp") {
		t.Errorf("expected existing host path /host/emptydir in bind arg, got:\n%s", got)
	}
	if strings.Contains(got, "hook-tmp") {
		t.Errorf("should not create hook-tmp when /tmp is already bound, got:\n%s", got)
	}
	// Should NOT emit mkdir -p hook-tmp
	if strings.Contains(got, "mkdir") {
		t.Errorf("should not emit mkdir when /tmp already bound, got:\n%s", got)
	}
}

func TestGeneratePostStartScript_CreatesHookTmpWhenNoTmpMount(t *testing.T) {
	// No /tmp volume mount → should create hook-tmp and use hookTmpBindMountArg
	cmd := ContainerCommand{
		containerName:  "app",
		containerImage: "docker://python:3.11-alpine",
		runtimeCommand: []string{
			"singularity", "exec", "--containall",
			"--bind /host/data:/data",
			"docker://python:3.11-alpine",
		},
		postStartHook: &LifecycleHookSpec{
			Type:        LifecycleHookTypeExec,
			ExecCommand: []string{"/bin/sh", "-c", "echo hook > /tmp/marker"},
		},
	}
	got := generatePostStartScript(testSlurmConfig(), cmd)
	if got == "" {
		t.Fatal("generatePostStartScript returned empty string")
	}
	if !strings.Contains(got, "mkdir") {
		t.Errorf("expected mkdir -p hook-tmp, got:\n%s", got)
	}
	if !strings.Contains(got, "hook-tmp") {
		t.Errorf("expected hook-tmp bind mount, got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// injectSocketDir
// ---------------------------------------------------------------------------

// Image and runtime names shared by the socket directory tests.
const (
	busyboxImage       = "docker://busybox"
	singularityRuntime = "singularity"
	enrootRuntime      = "enroot"
)

func TestInjectSocketDir(t *testing.T) {
	const mounts = "--bind /host/data:/data"
	tests := []struct {
		runtime string
		cmd     []string
		want    []string
	}{
		{
			runtime: singularityRuntime,
			cmd:     []string{singularityRuntime, "exec", mounts, busyboxImage},
			want: []string{singularityRuntime, "exec", mounts,
				"--bind", socketTestDir + ":" + socketTestDir, "--env", "INTERLINK_SOCKET_DIR=" + socketTestDir,
				busyboxImage},
		},
		{
			runtime: enrootRuntime,
			cmd:     []string{enrootRuntime, "start", "--rw", "", "mainpod-uid"},
			want: []string{enrootRuntime, "start", "--rw", "",
				"--mount", socketTestDir + ":" + socketTestDir, "--env", "INTERLINK_SOCKET_DIR=" + socketTestDir,
				"mainpod-uid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.runtime, func(t *testing.T) {
			got := injectSocketDir(tt.cmd, tt.runtime, socketTestDir)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("injectSocketDir() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestInjectSocketDir_NoDirectory(t *testing.T) {
	cmd := []string{singularityRuntime, "exec", busyboxImage}
	if got := injectSocketDir(cmd, singularityRuntime, ""); len(got) != len(cmd) {
		t.Errorf("injectSocketDir() without a directory changed the command: %q", got)
	}
	if got := injectSocketDir(nil, singularityRuntime, socketTestDir); len(got) != 0 {
		t.Errorf("injectSocketDir() on an empty command = %q, want it empty", got)
	}
}

func TestSocketDirExecArgsExpandOnlyWithADirectory(t *testing.T) {
	run := func(env ...string) string {
		cmd := exec.Command("bash", "-c", `set -- `+socketDirExecArgs+`; for a; do printf '%s\n' "$a"; done`)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("bash: %v", err)
		}
		return string(out)
	}
	if got := run(); got != "" {
		t.Errorf("without INTERLINK_SOCKET_DIR: got %q, want no arguments", got)
	}
	want := "--bind\n/s/1b2c:/s/1b2c\n--env\nINTERLINK_SOCKET_DIR=/s/1b2c\n"
	if got := run("INTERLINK_SOCKET_DIR=/s/1b2c"); got != want {
		t.Errorf("with INTERLINK_SOCKET_DIR: got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// postStart hook in the rendered job.sh
// ---------------------------------------------------------------------------

// fakeEnrootStart stands in for enroot: import and create do nothing, and start reads the
// options job.sh uses as enroot 4.2.1 does (enroot.in, enroot::start) and, like enroot,
// prints its usage and exits 1 on any other option. It checks --mount and --env as
// runtime::start does and logs one line per start to $FAKE_ENROOT_LOG.
const fakeEnrootStart = `#!/bin/bash
set -euo pipefail
[ "$1" = start ] || exit 0
shift
rc= mounts= envs=
while [ $# -gt 0 ]; do
  case "$1" in
  -c|--conf) shift 2 ;;
  --rc) rc=$(cat "$2"); echo "$2" >> "$FAKE_ENROOT_LOG.rc"; shift 2 ;;
  -m|--mount) [[ "$2" =~ ^[^[:space:]]+(:[^[:space:]]+)*$ ]] || exit 1; mounts+=" $2"; shift 2 ;;
  -e|--env) [[ "$2" =~ ^[[:alpha:]_][[:alnum:]_]*(=|$) ]] || exit 1; envs+=" $2"; shift 2 ;;
  -r|--root|-w|--rw) shift ;;
  --) shift; break ;;
  -?*) echo "Usage: enroot start [options] [--] NAME|IMAGE [COMMAND] [ARG...]" >&2; exit 1 ;;
  *) break ;;
  esac
done
name=$1; shift
printf 'start %s rc=%s mounts=%s env=%s cmd=%s\n' "$name" "$rc" "${mounts# }" "${envs# }" "$(printf '[%s]' "$@")" >> "$FAKE_ENROOT_LOG"
`

// fakeSingularityLog logs that singularity ran.
const fakeSingularityLog = `#!/bin/bash
echo "singularity $*" >> "$FAKE_ENROOT_LOG"
`

func postStartTestPod() v1.Pod {
	return v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hookpod", Namespace: metav1.NamespaceDefault, UID: socketTestUID}}
}

// postStartTestScript is the shell script of the postStart hook in the job.sh tests.
const postStartTestScript = "echo hook > /tmp/marker"

func postStartTestHook() *LifecycleHookSpec {
	return &LifecycleHookSpec{Type: LifecycleHookTypeExec, ExecCommand: []string{"/bin/sh", "-c", postStartTestScript}}
}

// Under enroot, the postStart exec hook runs in the pod's enroot container with enroot
// start, with an rc that runs the hook instead of the image's entrypoint, and the hook and
// the container share /tmp through enroot's --mount: hook-tmp, or the container's own /tmp
// volume. job.sh runs with a fake enroot that rejects options enroot does not know.
func TestPostStartHookEnrootJobScript(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	for name, script := range map[string]string{"enroot": fakeEnrootStart, "singularity": fakeSingularityLog} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // the fake runtimes must be executable
			t.Fatal(err)
		}
	}
	config := SlurmConfig{
		ContainerRuntime:     enrootRuntime,
		EnrootPath:           filepath.Join(bin, "enroot"),
		EnrootDefaultOptions: []string{"--rw"},
		SingularityPath:      filepath.Join(bin, "singularity"),
	}
	pod := postStartTestPod()
	enrootName := testContainer + string(pod.UID)
	const socketDirMount = socketTestDir + ":" + socketTestDir

	for _, tc := range []struct {
		name, mounts, tmpMount string
		hookTmp                bool
	}{
		{name: "hook-tmp", tmpMount: "WORKDIR/hook-tmp:/tmp", hookTmp: true},
		{name: "tmp volume", mounts: " --mount /host/emptydir:/tmp ", tmpMount: "/host/emptydir:/tmp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			commands := []ContainerCommand{{
				runtimeCommand: append(prepareRuntimeCommand(config, v1.Container{}, pod.ObjectMeta), tc.mounts, enrootName),
				containerName:  testContainer,
				containerImage: busyboxImage,
				postStartHook:  postStartTestHook(),
			}}
			if _, err := produceSLURMScript(context.Background(), config, pod, workDir, pod.ObjectMeta, commands, ResourceLimits{CPU: 1, Memory: 1024 * 1024}, false, false, nil); err != nil {
				t.Fatalf("produceSLURMScript() error: %v", err)
			}

			logFile := filepath.Join(t.TempDir(), "log")
			nodeTmp := t.TempDir()
			cmd := exec.Command("bash", "job.sh")
			cmd.Dir = workDir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "FAKE_ENROOT_LOG=" + logFile, "INTERLINK_SOCKET_DIR=" + socketTestDir, "TMPDIR=" + nodeTmp}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("job.sh failed: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			// enroot copies the --rc file preserving permissions, which failed from a
			// site's shared job filesystem: the file must be in TMPDIR, and removed after.
			rcPaths, err := os.ReadFile(logFile + ".rc")
			if err != nil {
				t.Fatal(err)
			}
			rcPath := strings.TrimSpace(string(rcPaths))
			if filepath.Dir(rcPath) != nodeTmp {
				t.Errorf("--rc file %s, want it in TMPDIR %s, not in the job directory", rcPath, nodeTmp)
			}
			if _, err := os.Stat(rcPath); err == nil { //nolint:gosec // the path the fake enroot logged
				t.Errorf("--rc file %s left behind", rcPath)
			}

			tmpMount := strings.ReplaceAll(tc.tmpMount, "WORKDIR", workDir)
			want := []string{
				"start " + enrootName + ` rc=exec "$@" mounts=` + tmpMount + " " + socketDirMount +
					" env=INTERLINK_SOCKET_DIR=" + socketTestDir + " cmd=[timeout][30][/bin/sh][-c][" + postStartTestScript + "]",
				"start " + enrootName + " rc= mounts=" + tmpMount + " env= cmd=[]",
			}
			if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("runtime calls\n%q\nwant\n%q", got, want)
			}
			if _, err := os.Stat(filepath.Join(workDir, "hook-tmp")); (err == nil) != tc.hookTmp {
				t.Errorf("hook-tmp created: %v, want %v", err == nil, tc.hookTmp)
			}
		})
	}
}

// Under enroot, a postStart httpGet hook runs curl on the host, and the container starts
// with hook-tmp mounted at /tmp through enroot's --mount, which enroot start accepts.
func TestPostStartHTTPGetHookEnrootJobScript(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	for name, script := range map[string]string{"enroot": fakeEnrootStart, "curl": "#!/bin/bash\necho \"curl $*\" >> \"$FAKE_ENROOT_LOG\"\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // the fakes must be executable
			t.Fatal(err)
		}
	}
	config := SlurmConfig{
		ContainerRuntime:     enrootRuntime,
		EnrootPath:           filepath.Join(bin, "enroot"),
		EnrootDefaultOptions: []string{"--rw"},
	}
	pod := postStartTestPod()
	enrootName := testContainer + string(pod.UID)
	workDir := t.TempDir()
	commands := []ContainerCommand{{
		runtimeCommand: append(prepareRuntimeCommand(config, v1.Container{}, pod.ObjectMeta), "", enrootName),
		containerName:  testContainer,
		containerImage: busyboxImage,
		postStartHook:  &LifecycleHookSpec{Type: LifecycleHookTypeHTTPGet, HTTPGet: &LifecycleHTTPGetSpec{Scheme: "http", Host: "localhost", Port: 8080, Path: "/ready"}},
	}}
	if _, err := produceSLURMScript(context.Background(), config, pod, workDir, pod.ObjectMeta, commands, ResourceLimits{CPU: 1, Memory: 1024 * 1024}, false, false, nil); err != nil {
		t.Fatalf("produceSLURMScript() error: %v", err)
	}
	logFile := filepath.Join(t.TempDir(), "log")
	cmd := exec.Command("bash", "job.sh")
	cmd.Dir = workDir
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FAKE_ENROOT_LOG=" + logFile}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("job.sh failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"curl -f -s --max-time 10 http://localhost:8080/ready",
		"start " + enrootName + " rc= mounts=" + workDir + "/hook-tmp:/tmp env= cmd=[]",
	}
	if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls\n%q\nwant\n%q", got, want)
	}
}

// Under Singularity, job.sh runs the postStart exec hook with singularity exec and binds
// the same /tmp into the container.
func TestPostStartHookSingularityJobScript(t *testing.T) {
	config := SlurmConfig{
		ContainerRuntime:          singularityRuntime,
		SingularityPath:           "/usr/bin/singularity",
		SingularityDefaultOptions: []string{"--containall"},
	}
	pod := postStartTestPod()
	workDir := t.TempDir()
	commands := []ContainerCommand{{
		runtimeCommand: append(prepareRuntimeCommand(config, v1.Container{}, pod.ObjectMeta), "", busyboxImage),
		containerName:  testContainer,
		containerImage: busyboxImage,
		postStartHook:  postStartTestHook(),
	}}
	if _, err := produceSLURMScript(context.Background(), config, pod, workDir, pod.ObjectMeta, commands, ResourceLimits{CPU: 1, Memory: 1024 * 1024}, false, false, nil); err != nil {
		t.Fatalf("produceSLURMScript() error: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(workDir, "job.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`mkdir -p "${workingPath}/hook-tmp"`,
		`/usr/bin/singularity exec --containall --bind "${workingPath}/hook-tmp:/tmp" ` + socketDirExecArgs +
			` docker://busybox timeout 30 /bin/sh -c '` + postStartTestScript + `' >> "${workingPath}/run-main.out" 2>&1 || true`,
		`runCtn main /usr/bin/singularity run --containall    --bind "${workingPath}/hook-tmp:/tmp" docker://busybox`,
	} {
		if !slices.Contains(strings.Split(string(raw), "\n"), line) {
			t.Errorf("job.sh has no line\n%s\nin\n%s", line, raw)
		}
	}
}
