package slurm

import (
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestExpandContainerArgs(t *testing.T) {
	env := []v1.EnvVar{
		{Name: "PORT", Value: "8080"},
		{Name: "TOKEN", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Key: "t"}}},
	}
	cases := map[string]string{
		"--port=$(PORT)": "--port=8080",
		"echo completed > /var/log/katib/$$$$.pid": "echo completed > /var/log/katib/$$.pid",
		"$$(PORT)":                "$(PORT)",
		"$(MISSING) and $(TOKEN)": "$(MISSING) and $(TOKEN)",
		"$HOME ${HOME} cost $5":   "$HOME ${HOME} cost $5",
		"trailing $":              "trailing $",
		"open $(PORT":             "open $(PORT",
		"no references":           "no references",
	}
	for in, want := range cases {
		got := expandContainerArgs([]string{in}, env)
		if len(got) != 1 || got[0] != want {
			t.Errorf("expand %q = %q, want %q", in, got, want)
		}
	}
	if got := expandContainerArgs(nil, env); got != nil {
		t.Errorf("nil args must stay nil, got %q", got)
	}
}
