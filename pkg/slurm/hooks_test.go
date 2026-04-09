package slurm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestExecuteExecHook_Success verifies that a valid exec hook command runs without error.
func TestExecuteExecHook_Success(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{"echo", "hello"})
	if err != nil {
		t.Errorf("executeExecHook with valid command returned unexpected error: %v", err)
	}
}

// TestExecuteExecHook_Failure verifies that a failing command returns an error.
func TestExecuteExecHook_Failure(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{"false"})
	if err == nil {
		t.Error("executeExecHook with failing command expected error, got nil")
	}
}

// TestExecuteExecHook_EmptyCommand verifies that an empty command slice returns an error.
func TestExecuteExecHook_EmptyCommand(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{})
	if err == nil {
		t.Error("executeExecHook with empty command expected error, got nil")
	}
}

// TestExecuteExecHook_NotFound verifies that a non-existent binary returns an error.
func TestExecuteExecHook_NotFound(t *testing.T) {
	ctx := context.Background()
	err := executeExecHook(ctx, []string{"/nonexistent/binary"})
	if err == nil {
		t.Error("executeExecHook with non-existent binary expected error, got nil")
	}
}

// TestExecuteHTTPGetHook_Success verifies that a successful HTTP response (2xx) is handled correctly.
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

// TestExecuteHTTPGetHook_404 verifies that a 4xx response is treated as an error.
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

// TestExecuteHTTPGetHook_ConnectionRefused verifies that an unreachable server returns an error.
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

// TestExecuteHTTPGetHook_DefaultsApplied verifies that empty scheme, host, and path use defaults.
func TestExecuteHTTPGetHook_DefaultsApplied(t *testing.T) {
	// A server that checks the request path
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

// TestExecuteLifecycleHook_Exec verifies exec dispatch through executeLifecycleHook.
func TestExecuteLifecycleHook_Exec(t *testing.T) {
	ctx := context.Background()
	handler := &v1.LifecycleHandler{
		Exec: &v1.ExecAction{Command: []string{"true"}},
	}
	if err := executeLifecycleHook(ctx, handler); err != nil {
		t.Errorf("executeLifecycleHook(exec) returned unexpected error: %v", err)
	}
}

// TestExecuteLifecycleHook_HTTPGet verifies httpGet dispatch through executeLifecycleHook.
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

// TestExecuteLifecycleHook_Nil verifies that a nil handler returns an error.
func TestExecuteLifecycleHook_Nil(t *testing.T) {
	ctx := context.Background()
	if err := executeLifecycleHook(ctx, nil); err == nil {
		t.Error("executeLifecycleHook(nil) expected error, got nil")
	}
}

// TestExecuteLifecycleHook_Unsupported verifies that an empty handler (no exec/httpGet) returns an error.
func TestExecuteLifecycleHook_Unsupported(t *testing.T) {
	ctx := context.Background()
	handler := &v1.LifecycleHandler{} // no exec, no httpGet
	if err := executeLifecycleHook(ctx, handler); err == nil {
		t.Error("executeLifecycleHook with empty handler expected error, got nil")
	}
}

// TestExecutePreStopHooks_NoHooks verifies that a pod with no lifecycle hooks is a no-op.
func TestExecutePreStopHooks_NoHooks(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app", Image: "busybox"},
			},
		},
	}
	// Should not panic or return an error
	executePreStopHooks(ctx, pod)
}

// TestExecutePreStopHooks_WithExecHook verifies that a preStop exec hook is executed.
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
	// Should complete without panic; errors are only logged, not returned
	executePreStopHooks(ctx, pod)
}

// TestExecutePreStopHooks_FailingHookDoesNotBlock verifies that a failing preStop hook
// does not block execution (errors are logged but non-fatal).
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
					// No lifecycle hooks
				},
			},
		},
	}
	// Should not panic even when the hook fails
	executePreStopHooks(ctx, pod)
}

// extractPort parses the port number from a test server URL (e.g. "http://127.0.0.1:12345").
func extractPort(t *testing.T, rawURL string) int {
	t.Helper()
	// rawURL is like "http://127.0.0.1:PORT"
	var port int
	if _, err := fmt.Sscanf(rawURL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("failed to extract port from URL %q: %v", rawURL, err)
	}
	return port
}
