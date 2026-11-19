package slurm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gangEndFakeSrun runs `srun [options] <command>` on the local host, with the
// command's output in the files that -o and -e name.
const gangEndFakeSrun = `#!/bin/bash
out= err=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -e) err=$2; shift 2 ;;
    -w) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
[ -z "$out" ] || exec >"$out"
[ -z "$err" ] || exec 2>"$err"
exec "$@"
`

// skipWithoutBash skips a test that runs a rendered script.
func skipWithoutBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

// runGangLocally runs the gang job of entry on the local host as a two-node
// allocation: srun runs each step here, sleep returns at once (the 60-probe
// readiness barrier would take two minutes) and body gives each member's job.sh.
func runGangLocally(t *testing.T, dataRoot string, entry *GangEntry, ordered []*BufferedMember, body map[*BufferedMember]string) ([]byte, error) {
	t.Helper()
	for m, s := range body {
		if err := os.WriteFile(filepath.Join(m.WorkDir, "job.sh"), []byte("#!/bin/bash\n"+s), 0o755); err != nil { //nolint:gosec // a fake executable
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	for name, s := range map[string]string{
		"srun":     gangEndFakeSrun,
		"scontrol": "#!/bin/sh\nprintf 'n0\\nn1\\n'\n",
		"hostname": "#!/bin/sh\nif [ \"$1\" = --ip-address ]; then echo 127.0.0.1; else echo localhost; fi\n",
		"sleep":    "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(s), 0o755); err != nil { //nolint:gosec // a fake executable
			t.Fatal(err)
		}
	}
	path, err := produceGangSLURMScript(context.Background(), SlurmConfig{BashPath: testBashPath, DataRootFolder: dataRoot}, entry, ordered)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SLURM_JOB_NODELIST=n[0-1]")
	return cmd.CombinedOutput()
}

// With interlink.eu/gang-end: all on the head, a gang without a launcher runs
// every rank to its end: a worker that outlives the head is not stopped with it,
// its pod reports its own exit code, and the gang fails with it.
func TestGangEndAll(t *testing.T) {
	skipWithoutBash(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	dataRoot := t.TempDir() + string(os.PathSeparator)
	head := mkMember(t, dataRoot, "h", map[string]string{"interlink.eu/gang-end": "all"})
	worker := mkMember(t, dataRoot, "w", nil)
	entry, ordered := gangOf("g1", head, worker)
	status := func(m *BufferedMember) string {
		return "'" + filepath.Join(m.WorkDir, "run-"+testGangContainer+".status") + "'"
	}
	out, err := runGangLocally(t, dataRoot, entry, ordered, map[*BufferedMember]string{
		head:   "echo head done\necho 0 > " + status(head) + "\n",
		worker: sleep + " 1\necho worker done\necho 3 > " + status(worker) + "\nexit 3\n",
	})
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("gang exit %v, want 3 (the worker's)\n%s", err, out)
	}
	for m, want := range map[*BufferedMember]int32{head: 0, worker: 3} {
		code, found, rerr := readMemberContainerExitCode(m.WorkDir, testGangContainer)
		if rerr != nil || !found || code != want {
			t.Errorf("pod %s: exit code %d (found %v, err %v), want %d", m.PodUID, code, found, rerr, want)
		}
	}
	if b, rerr := os.ReadFile(filepath.Join(worker.WorkDir, "job.out")); rerr != nil || !strings.Contains(string(b), "worker done") {
		t.Errorf("the worker did not run to its end: job.out %q (%v)", b, rerr)
	}

	// Without the annotation the head drives: workers wait for it, then stop with it.
	plain := mkMember(t, dataRoot, "p", nil)
	entry, ordered = gangOf("g2", plain, mkMember(t, dataRoot, "q", nil))
	if s := gangScript(t, SlurmConfig{BashPath: testBashPath}, entry, ordered); !strings.Contains(s, "readiness barrier") || !strings.Contains(s, "DRIVER_RC") {
		t.Errorf("the default launch lost its barrier or driver:\n%s", s)
	}
}
