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

// translateLifecycleHook converts a Kubernetes LifecycleHandler into an internal
// LifecycleHookSpec.  Returns nil when the handler is nil or unsupported.
func translateLifecycleHook(handler *v1.LifecycleHandler) *LifecycleHookSpec {
	if handler == nil {
		return nil
	}

	if handler.Exec != nil && len(handler.Exec.Command) > 0 {
		return &LifecycleHookSpec{
			Type:        LifecycleHookTypeExec,
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
		return &LifecycleHookSpec{
			Type: LifecycleHookTypeHTTPGet,
			HTTPGet: &LifecycleHTTPGetSpec{
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
// Exec-type hooks are dispatched via the container runtime (singularity exec)
// with a 30-second timeout, consistent with executeExecProbe in probes.go.
// If no container runtime is configured, they fall back to host-side execution.
//
// The returned string is empty when no container has a preStop hook.
func generatePreStopTrap(config SlurmConfig, commands []ContainerCommand) string {
	// Collect only non-init containers that carry a hook.
	type entry struct {
		name      string
		hook      *LifecycleHookSpec
		imageName string // fully-qualified image for container-runtime dispatch
	}
	var entries []entry
	for _, cmd := range commands {
		if cmd.isInitContainer || cmd.preStopHook == nil {
			continue
		}
		entries = append(entries, entry{
			name:      cmd.containerName,
			hook:      cmd.preStopHook,
			imageName: cmd.containerImage,
		})
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
		case LifecycleHookTypeExec:
			quotedArgs := make([]string, len(e.hook.ExecCommand))
			for i, arg := range e.hook.ExecCommand {
				quotedArgs[i] = shellescape.Quote(arg)
			}
			if e.imageName != "" && config.SingularityPath != "" {
				// Run inside the container via singularity exec — consistent with executeExecProbe
				parts := []string{shellescape.Quote(config.SingularityPath), "exec"}
				for _, opt := range config.SingularityDefaultOptions {
					parts = append(parts, shellescape.Quote(opt))
				}
				parts = append(parts, shellescape.Quote(e.imageName), "timeout", "30")
				parts = append(parts, quotedArgs...)
				sb.WriteString(fmt.Sprintf("  %s >> %s 2>&1 || true\n",
					strings.Join(parts, " "), outFile))
			} else {
				// Fallback: run on the host when singularity is not configured
				sb.WriteString(fmt.Sprintf("  timeout 30 %s >> %s 2>&1 || true\n",
					strings.Join(quotedArgs, " "), outFile))
			}

		case LifecycleHookTypeHTTPGet:
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

// hookTmpBindMountArg is the shell token for the --bind argument that shares
// a dedicated working-directory sub-folder as /tmp between the postStart hook
// invocation and the main container launch.
//
// The outer double-quotes are intentional: they allow ${workingPath} to be
// expanded by the shell at job-script runtime while preserving the bind-spec
// as a single argument even when workingPath contains spaces.
//
// This token must only be appended to a []string that will be joined into a
// POSIX shell command line (i.e. with strings.Join(..., " ")).  It must not
// be passed through shellescape.Quote because that would escape the $ and
// break variable expansion.
//
// When --containall is used (the default), singularity isolates /tmp, so
// without an explicit bind both the hook and the container would see their own
// private /tmp and the marker file would never be found by the container.
const hookTmpBindMountArg = `"${workingPath}/hook-tmp:/tmp"`

// generatePostStartScript generates a shell-script fragment that runs a container's
// postStart lifecycle hook synchronously before the container is launched.
//
// The hook runs inside the container via singularity exec (consistent with
// executeExecProbe in probes.go) with a 30-second timeout.  If no container
// runtime is configured, it falls back to host-side execution.
// Output is appended to the container's run-<name>.out log so it appears in
// kubectl logs.
//
// When singularity is used the hook and the container both receive a shared
// /tmp via --bind "${workingPath}/hook-tmp:/tmp" so that hook-created files
// (e.g. marker files) are visible to the container's entrypoint.
//
// Returns an empty string when the container has no postStart hook or is an
// init container.
func generatePostStartScript(config SlurmConfig, cmd ContainerCommand) string {
	if cmd.isInitContainer || cmd.postStartHook == nil {
		return ""
	}

	imageName := cmd.containerImage
	outFile := fmt.Sprintf(`"${workingPath}/run-%s.out"`, cmd.containerName)

	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("\n# postStart lifecycle hook for container %s\n", cmd.containerName))
	// Create the shared /tmp directory used by both the hook and the container.
	sb.WriteString(`mkdir -p "${workingPath}/hook-tmp"` + "\n")
	sb.WriteString(fmt.Sprintf(
		`printf "%%s\n" "$(date -Is --utc) Running postStart hook for container %s..." >> %s 2>&1`+"\n",
		cmd.containerName, outFile,
	))

	switch cmd.postStartHook.Type {
	case LifecycleHookTypeExec:
		quotedArgs := make([]string, len(cmd.postStartHook.ExecCommand))
		for i, arg := range cmd.postStartHook.ExecCommand {
			quotedArgs[i] = shellescape.Quote(arg)
		}
		if imageName != "" && config.SingularityPath != "" {
			// Run inside the container via singularity exec — consistent with executeExecProbe.
			// Add --bind "${workingPath}/hook-tmp:/tmp" so the hook writes to the shared /tmp
			// that is also mounted in the main container launch (see prepare.go).
			parts := []string{shellescape.Quote(config.SingularityPath), "exec"}
			for _, opt := range config.SingularityDefaultOptions {
				parts = append(parts, shellescape.Quote(opt))
			}
			parts = append(parts, "--bind", hookTmpBindMountArg)
			parts = append(parts, shellescape.Quote(imageName), "timeout", "30")
			parts = append(parts, quotedArgs...)
			sb.WriteString(fmt.Sprintf("%s >> %s 2>&1 || true\n",
				strings.Join(parts, " "), outFile))
		} else {
			// Fallback: run on the host when singularity is not configured
			sb.WriteString(fmt.Sprintf("timeout 30 %s >> %s 2>&1 || true\n",
				strings.Join(quotedArgs, " "), outFile))
		}

	case LifecycleHookTypeHTTPGet:
		url := fmt.Sprintf("%s://%s:%d%s",
			cmd.postStartHook.HTTPGet.Scheme,
			cmd.postStartHook.HTTPGet.Host,
			cmd.postStartHook.HTTPGet.Port,
			cmd.postStartHook.HTTPGet.Path,
		)
		sb.WriteString(fmt.Sprintf("curl -f -s --max-time 10 %s >> %s 2>&1 || true\n",
			shellescape.Quote(url), outFile))
	}

	sb.WriteString(fmt.Sprintf(
		`printf "%%s\n" "$(date -Is --utc) postStart hook for container %s completed." >> %s 2>&1`+"\n",
		cmd.containerName, outFile,
	))

	return sb.String()
}

// injectTmpBindMount inserts "--bind" hookTmpBindMountArg before the last
// element (the container image) in a runtime command slice.
//
// This is used to ensure the main container sees the same /tmp as the postStart
// hook when singularity's --containall flag is in effect.
//
// If runtimeCmd is empty (which indicates a misconfiguration since the runtime
// command should always contain at least the image), a warning is logged and
// the original slice is returned unchanged.
func injectTmpBindMount(runtimeCmd []string) []string {
	if len(runtimeCmd) == 0 {
		log.G(context.Background()).Warning("injectTmpBindMount: runtimeCmd is empty; skipping /tmp bind mount injection")
		return runtimeCmd
	}
	result := make([]string, 0, len(runtimeCmd)+2)
	result = append(result, runtimeCmd[:len(runtimeCmd)-1]...)
	result = append(result, "--bind", hookTmpBindMountArg)
	result = append(result, runtimeCmd[len(runtimeCmd)-1])
	return result
}