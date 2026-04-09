package slurm

import (
	"context"
	"fmt"
	"strings"

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