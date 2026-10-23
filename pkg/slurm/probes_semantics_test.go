package slurm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// runProbeHarness runs the generated probe script for the given probes for some
// seconds, next to a sleep standing for the container, and returns the harness output.
func runProbeHarness(t *testing.T, dir string, readiness, liveness []ProbeCommand, seconds int) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	singularity := filepath.Join(dir, "singularity")
	if err := os.WriteFile(singularity, []byte(fakeSingularity), 0o700); err != nil { //nolint:gosec // the fake runtime must be executable
		t.Fatal(err)
	}

	script := generateProbeScript(context.Background(), SlurmConfig{SingularityPath: singularity}, "main", "image.sif",
		readiness, liveness, nil)
	if strings.Contains(script, `waitForProbes "readiness"`) {
		t.Fatalf("readiness must not gate liveness:\n%s", script)
	}

	// The container is a sleep whose PID the shutdown would kill. The probe loops run
	// in the background with their output in a file; the whole process group goes at the end.
	harness := "workingPath=" + dir + "\nsleep 30 &\npidCtns=\"$!:main\"\n{\n" + script +
		"\n} >" + filepath.Join(dir, "probes.log") + " 2>&1\nsleep " + strconv.Itoa(seconds) +
		"\nkill -0 \"${pidCtns%:*}\" && echo CONTAINER_ALIVE\n"
	// Output goes to a file, not a pipe: the background loops would keep a pipe open.
	outFile, err := os.Create(filepath.Join(dir, "harness.out"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", harness)
	cmd.Stdout, cmd.Stderr = outFile, outFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Run()
	t.Cleanup(func() {
		if kerr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); kerr != nil {
			t.Logf("killing the probe loops: %v", kerr)
		}
	})
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
	return string(out)
}

// waitForStatus polls a probe status file until it holds want: the probe loops keep
// rewriting it, so a single read can see it truncated or between two writes.
func waitForStatus(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			got = strings.TrimSpace(string(b))
			if got == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
}

// TestReadinessFailureKeepsContainerRunning runs the generated probe script with a
// readiness probe that always fails and a liveness probe that passes. As in
// Kubernetes, the container must only be marked not ready: no shutdown, and the
// liveness probe runs although readiness never passed.
func TestReadinessFailureKeepsContainerRunning(t *testing.T) {
	dir := t.TempDir()
	out := runProbeHarness(t, dir, []ProbeCommand{execProbe("false")}, []ProbeCommand{execProbe("true")}, 4)
	if !strings.Contains(out, "CONTAINER_ALIVE") {
		t.Errorf("a failing readiness probe stopped the container:\n%s", out)
	}
	waitForStatus(t, filepath.Join(dir, "readiness-probe-main-0.status"), "FAILED_THRESHOLD")
	waitForStatus(t, filepath.Join(dir, "liveness-probe-main-0.status"), "SUCCESS")
}

// TestReadinessHonoursFailureThreshold checks that a ready container stays ready
// until failureThreshold consecutive failures. The probe passes once, then fails;
// on its third run it saves the status the first failure left behind.
func TestReadinessHonoursFailureThreshold(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "readiness-probe-main-0.status")
	// A script file: the generated script puts each probe argument in double quotes.
	check := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(check, []byte(`n=$(( $(cat `+dir+`/runs 2>/dev/null || echo 0) + 1 )); echo $n > `+dir+`/runs
[ $n -eq 3 ] && cp `+status+` `+dir+`/after-one-failure
[ $n -eq 1 ]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := execProbe("bash", check)
	probe.FailureThreshold = 3
	runProbeHarness(t, dir, []ProbeCommand{probe}, nil, 5)

	got, err := os.ReadFile(filepath.Join(dir, "after-one-failure"))
	if err != nil {
		probes, _ := os.ReadFile(filepath.Join(dir, "probes.log"))
		t.Fatalf("the probe did not run three times: %v\n%s", err, probes)
	}
	if s := strings.TrimSpace(string(got)); s != "SUCCESS" {
		t.Errorf("after one failure of three the status was %q, want SUCCESS", s)
	}
	waitForStatus(t, status, "FAILED_THRESHOLD")
}
