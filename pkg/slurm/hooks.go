package slurm

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
)

// executePreStopHooks runs the preStop lifecycle hooks for all containers in a pod.
// It is called before cancelling the SLURM job to give containers a chance to clean up.
// Errors are logged but do not prevent the job from being cancelled.
func executePreStopHooks(ctx context.Context, pod *v1.Pod) {
	for _, container := range pod.Spec.Containers {
		if container.Lifecycle == nil || container.Lifecycle.PreStop == nil {
			continue
		}
		log.G(ctx).Infof("Executing preStop hook for container %s in pod %s", container.Name, pod.Name)
		if err := executeLifecycleHook(ctx, container.Lifecycle.PreStop); err != nil {
			log.G(ctx).Errorf("preStop hook for container %s failed: %v", container.Name, err)
		}
	}
}

// executeLifecycleHook executes a single LifecycleHandler (exec or httpGet).
// Returns an error if the hook fails or if the handler type is unsupported.
func executeLifecycleHook(ctx context.Context, handler *v1.LifecycleHandler) error {
	if handler == nil {
		return fmt.Errorf("lifecycle handler is nil")
	}
	if handler.Exec != nil {
		return executeExecHook(ctx, handler.Exec.Command)
	}
	if handler.HTTPGet != nil {
		return executeHTTPGetHook(ctx, handler.HTTPGet)
	}
	return fmt.Errorf("unsupported lifecycle hook type: only exec and httpGet are supported")
}

// executeExecHook runs the given command locally as a preStop exec hook.
// The command is executed on the host running the plugin (not inside the SLURM container).
func executeExecHook(ctx context.Context, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("exec hook has empty command")
	}
	log.G(ctx).Infof("Executing preStop exec hook: %v", command)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // command comes from pod spec, which is trusted input
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec hook %v failed: %w (output: %s)", command, err, string(output))
	}
	log.G(ctx).Debugf("preStop exec hook output: %s", strings.TrimSpace(string(output)))
	return nil
}

// executeHTTPGetHook makes an HTTP GET request as a preStop httpGet hook.
// A 2xx or 3xx response is considered success; anything else is an error.
func executeHTTPGetHook(ctx context.Context, httpGet *v1.HTTPGetAction) error {
	if httpGet == nil {
		return fmt.Errorf("httpGet action is nil")
	}

	scheme := strings.ToLower(string(httpGet.Scheme))
	if scheme == "" {
		scheme = "http"
	}

	host := httpGet.Host
	if host == "" {
		host = "localhost"
	}

	path := httpGet.Path
	if path == "" {
		path = "/"
	}

	url := fmt.Sprintf("%s://%s:%d%s", scheme, host, httpGet.Port.IntVal, path)
	log.G(ctx).Infof("Executing preStop httpGet hook: GET %s", url)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // using client with timeout is sufficient here
	if err != nil {
		return fmt.Errorf("httpGet hook GET %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("httpGet hook GET %s returned non-success status: %d", url, resp.StatusCode)
	}

	log.G(ctx).Debugf("preStop httpGet hook GET %s succeeded with status: %d", url, resp.StatusCode)
	return nil
}
