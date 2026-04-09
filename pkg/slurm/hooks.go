package slurm

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// translatePreStopHook converts a Kubernetes LifecycleHandler into an internal
// PreStopHookSpec.  Returns nil when the handler is nil or unsupported.
func translatePreStopHook(handler *v1.LifecycleHandler) *PreStopHookSpec {
	if handler == nil {
		return nil
	}

	if handler.Exec != nil && len(handler.Exec.Command) > 0 {
		return &PreStopHookSpec{
			Type:        PreStopHookTypeExec,
			ExecCommand: handler.Exec.Command,
		}
	}

	if handler.HTTPGet != nil {
		// Named ports (string-typed) cannot be resolved outside the container runtime;
		// only numeric ports are supported for preStop httpGet hooks in the SLURM context.
		if handler.HTTPGet.Port.Type == intstr.String {
			log.G(context.Background()).Warningf("preStop httpGet hook uses a named port (%q) which cannot be resolved in the SLURM context; hook will be skipped", handler.HTTPGet.Port.StrVal)
			return nil
		}
		scheme := strings.ToLower(string(handler.HTTPGet.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		host := handler.HTTPGet.Host
		if host == "" {
			host = "localhost"
		}
		path := handler.HTTPGet.Path
		if path == "" {
			path = "/"
		}
		return &PreStopHookSpec{
			Type: PreStopHookTypeHTTPGet,
			HTTPGet: &PreStopHTTPGetSpec{
				Scheme: scheme,
				Host:   host,
				Port:   handler.HTTPGet.Port.IntVal,
				Path:   path,
			},
		}
	}

	return nil
}

// generatePreStopTrap generates the shell-script fragment that installs a SIGTERM
// trap in job.sh.  When scancel sends SIGTERM to the SLURM job, the trap runs
// each container's preStop lifecycle hook before forwarding the signal to the
// running container processes.
//
// The returned string is empty when no container has a preStop hook.
func generatePreStopTrap(commands []ContainerCommand) string {
	// Collect only non-init containers that carry a hook.
	type entry struct {
		name string
		hook *PreStopHookSpec
	}
	var entries []entry
	for _, cmd := range commands {
		if cmd.isInitContainer || cmd.preStopHook == nil {
			continue
		}
		entries = append(entries, entry{name: cmd.containerName, hook: cmd.preStopHook})
	}
	if len(entries) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("\n# PreStop lifecycle hooks — executed when the job receives SIGTERM (e.g. from scancel)\n")
	sb.WriteString("preStopTrap() {\n")
	sb.WriteString(`  printf "%s\n" "$(date -Is --utc) Received SIGTERM: running preStop lifecycle hooks..."` + "\n")

	for _, e := range entries {
		sb.WriteString(fmt.Sprintf(
			`  printf "%%s\n" "$(date -Is --utc) Running preStop hook for container %s..."`,
			e.name,
		) + "\n")

		outFile := fmt.Sprintf(`"${workingPath}/prestop-%s.out"`, e.name)

		switch e.hook.Type {
		case PreStopHookTypeExec:
			quotedArgs := make([]string, len(e.hook.ExecCommand))
			for i, arg := range e.hook.ExecCommand {
				quotedArgs[i] = shellescape.Quote(arg)
			}
			sb.WriteString(fmt.Sprintf("  %s >> %s 2>&1 || true\n",
				strings.Join(quotedArgs, " "), outFile))

		case PreStopHookTypeHTTPGet:
			url := fmt.Sprintf("%s://%s:%d%s",
				e.hook.HTTPGet.Scheme,
				e.hook.HTTPGet.Host,
				e.hook.HTTPGet.Port,
				e.hook.HTTPGet.Path,
			)
			sb.WriteString(fmt.Sprintf("  curl -f -s --max-time 10 %s >> %s 2>&1 || true\n",
				shellescape.Quote(url), outFile))
		}
	}

	sb.WriteString(`  printf "%s\n" "$(date -Is --utc) preStop hooks completed, terminating containers..."` + "\n")
	sb.WriteString("  for pidCtn in ${pidCtns} ; do\n")
	sb.WriteString("    pid=\"${pidCtn%:*}\"\n")
	sb.WriteString("    ctn=\"${pidCtn#*:}\"\n")
	sb.WriteString(`    printf "%s\n" "$(date -Is --utc) Sending SIGTERM to container ${ctn} pid ${pid}..."` + "\n")
	sb.WriteString("    kill \"${pid}\" 2>/dev/null || true\n")
	sb.WriteString("  done\n")
	sb.WriteString("  wait\n")
	sb.WriteString(`  printf "%s\n" "$(date -Is --utc) All containers terminated."` + "\n")
	sb.WriteString("}\n")
	sb.WriteString("trap preStopTrap SIGTERM\n")

	return sb.String()
}

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

	// Named ports cannot be resolved outside a container runtime.
	if httpGet.Port.Type == intstr.String {
		return fmt.Errorf("httpGet hook uses a named port (%q) which cannot be resolved in the SLURM context", httpGet.Port.StrVal)
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