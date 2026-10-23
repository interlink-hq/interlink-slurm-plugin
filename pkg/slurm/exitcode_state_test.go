package slurm

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestExitCodeFallbackFollowsJobState covers a job that ended before the container
// wrote its status file: Slurm's exit code can read 0 although the job was
// OOM-killed or failed, and the pod must not end Succeeded.
func TestExitCodeFallbackFollowsJobState(t *testing.T) {
	cases := []struct {
		state     string
		slurmCode string
		want      int32
	}{
		{"OOM", "0", 137},
		{"F", "0", 1},
		{"TO", "0", 1},
		{"F", "3", 3},
		{"CD", "0", 0},
	}
	for _, c := range cases {
		t.Run(c.state+"/"+c.slurmCode, func(t *testing.T) {
			dir := t.TempDir()
			got, err := getExitCode(context.Background(), dir, "main", c.slurmCode, c.state, "")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("exit code = %d, want %d", got, c.want)
			}
			// The recorded status must agree with what was reported.
			b, err := os.ReadFile(filepath.Join(dir, "init-main.status"))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != strconv.Itoa(int(c.want)) {
				t.Errorf("status file = %q, want %d", b, c.want)
			}
		})
	}
}
