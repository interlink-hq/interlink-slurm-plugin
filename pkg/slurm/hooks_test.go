package slurm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestTranslatePreStopHook_NoExecNoHTTPGet(t *testing.T) {
	handler := &v1.LifecycleHandler{} // neither exec nor httpGet
	if got := translatePreStopHook(handler); got != nil {
		t.Errorf("translatePreStopHook(empty handler) = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// generatePreStopTrap
// ---------------------------------------------------------------------------

func TestGeneratePreStopTrap_NoHooks(t *testing.T) {
	commands := []ContainerCommand{
		{containerName: "app", isInitContainer: false, preStopHook: nil},
	}
	if got := generatePreStopTrap(commands); got != "" {
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
	if got := generatePreStopTrap(commands); got != "" {
		t.Errorf("generatePreStopTrap should ignore init containers, got %q", got)
	}
}

func TestGeneratePreStopTrap_ExecHook(t *testing.T) {
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
	got := generatePreStopTrap(commands)
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
	got := generatePreStopTrap(commands)
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
	got := generatePreStopTrap(commands)
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
// executeExecHook
// ---------------------------------------------------------------------------

func TestExecuteExecHook_Success(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{"echo", "hello"})
	if err != nil {
		t.Errorf("executeExecHook with valid command returned unexpected error: %v", err)
	}
}

func TestExecuteExecHook_Failure(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{"false"})
	if err == nil {
		t.Error("executeExecHook with failing command expected error, got nil")
	}
}

func TestExecuteExecHook_EmptyCommand(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{})
	if err == nil {
		t.Error("executeExecHook with empty command expected error, got nil")
	}
}

func TestExecuteExecHook_NotFound(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{"/nonexistent/binary"})
	if err == nil {
		t.Error("executeExecHook with non-existent binary expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// executeHTTPGetHook
// ---------------------------------------------------------------------------

func TestExecuteHTTPGetHook_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	httpGet := &v1.HTTPGetAction{
		Scheme: "HTTP",
		Host:   "127.0.0.1",
		Port:   intstr.FromInt(extractPort(t, srv.URL)),
		Path:   "/",
	}
	err := executeHTTPGetHook(ctx, httpGet)
	if err != nil {
		t.Errorf("executeHTTPGetHook with 200 response returned unexpected error: %v", err)
	}
}

func TestExecuteHTTPGetHook_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ctx := context.Background()
	httpGet := &v1.HTTPGetAction{
		Scheme: "HTTP",
		Host:   "127.0.0.1",
		Port:   intstr.FromInt(extractPort(t, srv.URL)),
		Path:   "/",
	}
	err := executeHTTPGetHook(ctx, httpGet)
	if err == nil {
		t.Error("executeHTTPGetHook with 404 response expected error, got nil")
	}
}

func TestExecuteHTTPGetHook_ConnectionRefused(t *testing.T) {
	ctx := context.Background()
	httpGet := &v1.HTTPGetAction{
		Scheme: "HTTP",
		Host:   "127.0.0.1",
		Port:   intstr.FromInt(19999), // no server listening on this port
		Path:   "/",
	}
	err := executeHTTPGetHook(ctx, httpGet)
	if err == nil {
		t.Error("executeHTTPGetHook with connection refused expected error, got nil")
	}
}

func TestExecuteHTTPGetHook_DefaultsApplied(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	httpGet := &v1.HTTPGetAction{
		// Scheme, Host, and Path intentionally left empty to test defaults
		Port: intstr.FromInt(extractPort(t, srv.URL)),
	}
	err := executeHTTPGetHook(ctx, httpGet)
	if err != nil {
		t.Errorf("executeHTTPGetHook with defaults returned unexpected error: %v", err)
	}
	if gotPath != "/" {
		t.Errorf("expected default path '/', got %q", gotPath)
	}
}

// ---------------------------------------------------------------------------
// executeLifecycleHook
// ---------------------------------------------------------------------------

func TestExecuteLifecycleHook_Exec(t *testing.T) {
	ctx := context.Background()
	handler := &v1.LifecycleHandler{
		Exec: &v1.ExecAction{Command: []string{"true"}},
	}
	if err := executeLifecycleHook(ctx, handler); err != nil {
		t.Errorf("executeLifecycleHook(exec) returned unexpected error: %v", err)
	}
}

func TestExecuteLifecycleHook_HTTPGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx := context.Background()
	handler := &v1.LifecycleHandler{
		HTTPGet: &v1.HTTPGetAction{
			Scheme: "HTTP",
			Host:   "127.0.0.1",
			Port:   intstr.FromInt(extractPort(t, srv.URL)),
			Path:   "/",
		},
	}
	if err := executeLifecycleHook(ctx, handler); err != nil {
		t.Errorf("executeLifecycleHook(httpGet) returned unexpected error: %v", err)
	}
}

func TestExecuteLifecycleHook_Nil(t *testing.T) {
	ctx := context.Background()
	if err := executeLifecycleHook(ctx, nil); err == nil {
		t.Error("executeLifecycleHook(nil) expected error, got nil")
	}
}

func TestExecuteLifecycleHook_Unsupported(t *testing.T) {
	ctx := context.Background()
	handler := &v1.LifecycleHandler{} // no exec, no httpGet
	if err := executeLifecycleHook(ctx, handler); err == nil {
		t.Error("executeLifecycleHook with empty handler expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// executePreStopHooks
// ---------------------------------------------------------------------------

func TestExecutePreStopHooks_NoHooks(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app", Image: "busybox"},
			},
		},
	}
	executePreStopHooks(ctx, pod)
}

func TestExecutePreStopHooks_WithExecHook(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "app",
					Image: "busybox",
					Lifecycle: &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: []string{"echo", "prestop"},
							},
						},
					},
				},
			},
		},
	}
	executePreStopHooks(ctx, pod)
}

func TestExecutePreStopHooks_FailingHookDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "app",
					Image: "busybox",
					Lifecycle: &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: []string{"false"}, // always fails
							},
						},
					},
				},
				{
					Name:  "sidecar",
					Image: "busybox",
				},
			},
		},
	}
	// Should not panic even when the hook fails
	executePreStopHooks(ctx, pod)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// extractPort parses the port number from a test server URL (e.g. "http://127.0.0.1:12345").
func extractPort(t *testing.T, rawURL string) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(rawURL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("failed to extract port from URL %q: %v", rawURL, err)
	}
	return port
}
