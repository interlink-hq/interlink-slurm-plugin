package slurm

import (
	"strings"

	v1 "k8s.io/api/core/v1"
)

// expandContainerArgs applies to a container's command or args what the kubelet applies
// before it starts the container: $(VAR) becomes the value of the container's
// environment variable VAR, $$ becomes $, and a reference to an unknown variable is
// left as it is. Without it the job's shell sees the Kubernetes escapes: Katib, for
// one, writes $$$$ for the shell's own PID and gets the PID twice.
func expandContainerArgs(args []string, env []v1.EnvVar) []string {
	if len(args) == 0 {
		return args
	}
	vars := make(map[string]string, len(env))
	for _, e := range env {
		if e.ValueFrom == nil || e.Value != "" {
			vars[e.Name] = e.Value
		}
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = expandKubernetesVars(a, vars)
	}
	return out
}

// expandKubernetesVars follows k8s.io/kubernetes/third_party/forked/golang/expansion.
func expandKubernetesVars(input string, vars map[string]string) string {
	var buf strings.Builder
	checkpoint := 0
	for cursor := 0; cursor < len(input); cursor++ {
		if input[cursor] != '$' || cursor+1 >= len(input) {
			continue
		}
		buf.WriteString(input[checkpoint:cursor])
		rest := input[cursor+1:]
		advance := 1
		switch rest[0] {
		case '$':
			buf.WriteByte('$')
		case '(':
			end := strings.IndexByte(rest, ')')
			if end < 0 {
				buf.WriteString("$(")
				break
			}
			name := rest[1:end]
			if val, ok := vars[name]; ok {
				buf.WriteString(val)
			} else {
				buf.WriteString("$(" + name + ")")
			}
			advance = end + 1
		default:
			buf.WriteByte('$')
			buf.WriteByte(rest[0])
		}
		cursor += advance
		checkpoint = cursor + 1
	}
	return buf.String() + input[checkpoint:]
}
