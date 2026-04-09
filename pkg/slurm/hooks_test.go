package slurm

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ---------------------------------------------------------------------------
// translatePreStopHook
// ---------------------------------------------------------------------------

func TestTranslatePreStopHook_Nil(t *testing.T) {
	if got := translatePreStopHook(nil); got != nil {
		t.Errorf("translatePreStopHook(nil) = %v, want nil", got)
	}
}

func TestTranslatePreStopHook_ExecEmpty(t *testing.T) {
	handler := &v1.LifecycleHandler{
		Exec: &v1.ExecAction{Command: []string{}},
	}
	// Empty command slice should fall through and return nil (unsupported)
	if got := translatePreStopHook(handler); got != nil {
		t.Errorf("translatePreStopHook(exec with empty command) = %v, want nil", got)
	}
}

func TestTranslatePreStopHook_Exec(t *testing.T) {
	cmd := []string{"/bin/sh", "-c", "echo prestop"}
	handler := &v1.LifecycleHandler{
		Exec: &v1.ExecAction{Command: cmd},
	}
	got := translatePreStopHook(handler)
	if got == nil {
		t.Fatal("translatePreStopHook returned nil, expected PreStopHookSpec")
	}
	if got.Type != PreStopHookTypeExec {
		t.Errorf("Type = %q, want %q", got.Type, PreStopHookTypeExec)
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
	got := translatePreStopHook(handler)
	if got == nil {
		t.Fatal("translatePreStopHook returned nil, expected PreStopHookSpec")
	}
	if got.Type != PreStopHookTypeHTTPGet {
		t.Errorf("Type = %q, want %q", got.Type, PreStopHookTypeHTTPGet)
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
	got := translatePreStopHook(handler)
	if got == nil {
		t.Fatal("translatePreStopHook returned nil, expected PreStopHookSpec")
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
	if got := translatePreStopHook(handler); got != nil {
		t.Errorf("translatePreStopHook(named port) = %v, want nil", got)
	}
}

func TestTranslatePreStopHook_NoExecNoHTTPGet(t *testing.T) {
	handler := &v1.LifecycleHandler{} // neither exec nor httpGet
	if got := translatePreStopHook(handler); got != nil {
		t.Errorf("translatePreStopHook(empty handler) = %v, want nil", got)
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
			preStopHook: &PreStopHookSpec{
				Type:        PreStopHookTypeExec,
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
			// Simulate the runtimeCommand produced by prepareRuntimeCommand for singularity
			runtimeCommand: []string{"/usr/bin/singularity", "exec", "--nv", "docker://ubuntu:latest"},
			preStopHook: &PreStopHookSpec{
				Type:        PreStopHookTypeExec,
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
			preStopHook: &PreStopHookSpec{
				Type:        PreStopHookTypeExec,
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
			preStopHook: &PreStopHookSpec{
				Type: PreStopHookTypeHTTPGet,
				HTTPGet: &PreStopHTTPGetSpec{
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
			runtimeCommand:  []string{"/usr/bin/singularity", "exec", "--nv", "docker://ubuntu:latest"},
			preStopHook: &PreStopHookSpec{
				Type:        PreStopHookTypeExec,
				ExecCommand: []string{"echo", "bye-app"},
			},
		},
		{
			containerName:   "sidecar",
			isInitContainer: false,
			preStopHook: &PreStopHookSpec{
				Type: PreStopHookTypeHTTPGet,
				HTTPGet: &PreStopHTTPGetSpec{
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
