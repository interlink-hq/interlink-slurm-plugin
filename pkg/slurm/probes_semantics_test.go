package slurm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSingularity runs the probe command that follows "timeout <s>" in its arguments,
// so the generated probe script can run without a container runtime.
const fakeSingularity = `#!/bin/bash
while [ "$#" -gt 0 ] && [ "$1" != timeout ]; do shift; done
exec "$@"
`

func execProbe(cmd ...string) ProbeCommand {
	return ProbeCommand{
		Type: ProbeTypeExec, ExecAction: &ExecAction{Command: cmd},
		PeriodSeconds: 1, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 1,
	}
}

// TestReadinessFailureKeepsContainerRunning runs the generated probe script with a
// readiness probe that always fails and a liveness probe that passes. As in
// Kubernetes, the container must only be marked not ready: no shutdown, and the
// liveness probe runs although readiness never passed.
func TestReadinessFailureKeepsContainerRunning(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	singularity := filepath.Join(dir, "singularity")
	if err := os.WriteFile(singularity, []byte(fakeSingularity), 0o700); err != nil { //nolint:gosec // the fake runtime must be executable
		t.Fatal(err)
	}

	script := generateProbeScript(context.Background(), SlurmConfig{SingularityPath: singularity}, "main", "image.sif",
		[]ProbeCommand{execProbe("false")}, []ProbeCommand{execProbe("true")}, nil)
	if strings.Contains(script, `waitForProbes "readiness"`) {
		t.Fatalf("readiness must not gate liveness:\n%s", script)
	}

	// The container is a sleep whose PID the shutdown would kill. The probe loops run
	// in the background with their output in a file; the whole process group goes at the end.
	harness := "workingPath=" + dir + "\nsleep 30 &\npidCtns=\"$!:main\"\n{\n" + script +
		"\n} >" + filepath.Join(dir, "probes.log") + " 2>&1\nsleep 4\nkill -0 \"${pidCtns%:*}\" && echo CONTAINER_ALIVE\n"
	// Output goes to a file, not a pipe: the background loops would keep a pipe open.
	outFile, err := os.Create(filepath.Join(dir, "harness.out"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", harness)
	cmd.Stdout, cmd.Stderr = outFile, outFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Run()
	defer func() {
		if kerr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); kerr != nil {
			t.Logf("killing the probe loops: %v", kerr)
		}
	}()
	if cerr := outFile.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	out, rerr := os.ReadFile(outFile.Name())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CONTAINER_ALIVE") {
		t.Errorf("a failing readiness probe stopped the container:\n%s", out)
	}
	read := func(name string) string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err == nil || time.Now().After(deadline) {
				return strings.TrimSpace(string(b))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if got := read("readiness-probe-main-0.status"); got != "FAILED_THRESHOLD" {
		t.Errorf("readiness status = %q, want FAILED_THRESHOLD", got)
	}
	if got := read("liveness-probe-main-0.status"); got != "SUCCESS" {
		t.Errorf("liveness status = %q, want SUCCESS (liveness must start without readiness)", got)
	}
}
