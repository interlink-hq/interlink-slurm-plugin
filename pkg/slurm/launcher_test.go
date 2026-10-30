package slurm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Literals shared by the gang and launcher tests.
const (
	testBashPath         = "/bin/bash"
	testModuleLoad       = "module load singularity"
	testFlagsAnnotation  = "slurm-job.vk.io/flags"
	testMPIEnvAnnotation = "slurm-job.vk.io/mpi-env"
	testPython           = "python"
	testScript           = "t.py"
	testGres4            = "--gres=gpu:4"
	testNV               = "--nv"
	testTrainScript      = "train.py"
	testNamespace        = "default"
	testContainer        = "main"
)

// testRay is a launcher the test site does not define.
const testRay = "ray"

// testSite is a site file with a top level and two launchers, mpi marked for one
// Slurm task per GPU.
const testSite = `# test site
module load singularity
export SITE_LOADED=1

# interlink-tasks-per-gpu: mpi

torchrun-launcher() {
  srun --ntasks-per-node=1 --output="$GANG_LOG_DIR/rank-%n.out" $GANG_CONTAINER torchrun --nnodes="$GANG_NNODES" "$@"
}

function mpi-launcher {
  mpirun -n "$(( GANG_NNODES * GANG_GPUS_PER_NODE ))" $GANG_CONTAINER "$@"
}
`

func writeSite(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "site.sh")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGpusPerNodeFromFlags(t *testing.T) {
	cases := []struct {
		flags []string
		want  int
	}{
		{nil, 0},
		{[]string{testGres4}, 4},
		{[]string{"--gres=gpu:h100:2"}, 2},
		{[]string{"--gpus-per-node=3"}, 3},
		{[]string{"--gpus-per-node=h100:3"}, 3},
		{[]string{testGres4, "--gpus-per-node=2"}, 2},
		{[]string{"--gres=lscratch:10"}, 0},
	}
	for _, c := range cases {
		if got := gpusPerNodeFromFlags(c.flags); got != c.want {
			t.Errorf("gpusPerNodeFromFlags(%v) = %d, want %d", c.flags, got, c.want)
		}
	}
}

func TestLoadSite(t *testing.T) {
	ctx := context.Background()

	site, ok, err := loadSite(ctx, SlurmConfig{SiteScriptPath: writeSite(t, testSite)})
	if err != nil || !ok {
		t.Fatalf("loadSite: ok=%v err=%v", ok, err)
	}
	for _, name := range []string{"torchrun", "mpi"} {
		if err := site.checkLauncher(name); err != nil {
			t.Errorf("checkLauncher(%q): %v", name, err)
		}
	}
	if !site.TasksPerGPU["mpi"] || site.TasksPerGPU["torchrun"] {
		t.Errorf("TasksPerGPU = %v, want only mpi", site.TasksPerGPU)
	}
	for _, bad := range []string{testRay, "../etc/passwd", "bad name", ""} {
		if err := site.checkLauncher(bad); err == nil {
			t.Errorf("checkLauncher(%q) must fail", bad)
		}
	}

	if _, ok, err := loadSite(ctx, SlurmConfig{SiteScriptPath: filepath.Join(t.TempDir(), "none.sh")}); ok || err != nil {
		t.Errorf("a missing site file is not an error by itself: ok=%v err=%v", ok, err)
	}
	if _, hasBash := lookBash(); hasBash {
		if _, _, err := loadSite(ctx, SlurmConfig{SiteScriptPath: writeSite(t, "torchrun-launcher() {\n")}); err == nil {
			t.Error("a site file that does not parse must fail")
		}
	}
}

// lookBash reports whether a bash binary is on PATH (mirrors bashSyntaxCheck).
func lookBash() (string, bool) {
	for _, c := range []string{"bash", testBashPath, "/usr/bin/bash"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, true
		}
	}
	return "", false
}

func renderGang(t *testing.T, config SlurmConfig, extra map[string]string, cmd, args []string) (string, string) {
	t.Helper()
	dataRoot := t.TempDir() + string(os.PathSeparator)
	entry, ordered := gangOf("g1",
		mkGangMemberWithRC(t, dataRoot, "h", extra, cmd, args),
		mkGangMemberWithRC(t, dataRoot, "w", extra, cmd, args))
	return gangScript(t, config, entry, ordered), dataRoot
}

func TestProduceGangSLURMScriptSiteLauncher(t *testing.T) {
	config := SlurmConfig{BashPath: testBashPath, Commandprefix: testModuleLoad, SiteScriptPath: writeSite(t, testSite)}
	extra := map[string]string{LauncherAnnotation: "torchrun", testFlagsAnnotation: testGres4}
	s, dataRoot := renderGang(t, config, extra, []string{testPython}, []string{testTrainScript})

	head, worker := dataRoot+"default-h/", dataRoot+"default-w/"
	for _, want := range []string{
		"source " + head + siteFileName,          // site top level runs in the batch shell
		"torchrun-launcher python train.py ) >>", // called once with the head pod's command
		"export GANG_CONTAINER=",
		"export GANG_NNODES=2",
		"export GANG_GPUS_PER_NODE=4",
		`export GANG_NODES="${nodes[*]}"`,
		"export GANG_LOG_DIR=" + head + "ranks",
		"ln -sfn " + head + "ranks/rank-1.out " + worker + "run-main.out", // worker's log is rank 1's
		"#SBATCH --ntasks-per-node=1",
		`echo "$LAUNCHER_RC" > ` + worker + "run-main.status",
		// the worker's pod learns its node (rank 1, the second node) for its shadow
		`srun --overlap -N1 -n1 -w "${nodes[1]}" hostname -f > ` + worker + "compute-node",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("site launcher script missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "srun --overlap --nodes=1 --ntasks=1 -w \"${nodes[1]}\"") {
		t.Errorf("a site launcher starts the ranks itself: no per-pod srun expected\n---\n%s", s)
	}
	if _, err := os.Stat(head + siteFileName); err != nil {
		t.Errorf("site file not copied next to job.slurm: %v", err)
	}
}

func TestProduceGangSLURMScriptTasksPerGPU(t *testing.T) {
	config := SlurmConfig{BashPath: testBashPath, SiteScriptPath: writeSite(t, testSite)}
	extra := map[string]string{LauncherAnnotation: "mpi", testFlagsAnnotation: testGres4}
	s, _ := renderGang(t, config, extra, []string{"invert_test"}, nil)
	if !strings.Contains(s, "#SBATCH --ntasks-per-node=4") {
		t.Errorf("mpi is marked interlink-tasks-per-gpu: want one task per GPU\n---\n%s", s)
	}
}

func TestSiteLauncherFailsWithoutFallback(t *testing.T) {
	extra := map[string]string{LauncherAnnotation: testRay}
	dataRoot := t.TempDir() + string(os.PathSeparator)
	for name, config := range map[string]SlurmConfig{
		"no site file":         {BashPath: testBashPath, SiteScriptPath: filepath.Join(t.TempDir(), "none.sh")},
		"launcher not defined": {BashPath: testBashPath, SiteScriptPath: writeSite(t, testSite)},
	} {
		entry, ordered := gangOf("g1",
			mkGangMemberWithRC(t, dataRoot, "h", extra, []string{testRay}, nil),
			mkGangMemberWithRC(t, dataRoot, "w", extra, []string{testRay}, nil))
		if _, err := produceGangSLURMScript(context.Background(), config, entry, ordered); err == nil {
			t.Errorf("%s: the submission must fail, not fall back", name)
		}
	}
}

// Without a launcher annotation the site top level still runs, and each pod runs its
// own command (one srun per member).
func TestSiteTopLevelWithoutLauncher(t *testing.T) {
	config := SlurmConfig{BashPath: testBashPath, SiteScriptPath: writeSite(t, testSite)}
	s, dataRoot := renderGang(t, config, nil, []string{testPython}, []string{testScript})
	if !strings.Contains(s, "source "+dataRoot+"default-h/"+siteFileName) {
		t.Errorf("site file must be sourced for every gang\n---\n%s", s)
	}
	if !strings.Contains(s, `srun --overlap --nodes=1 --ntasks=1 -w "${nodes[1]}"`) {
		t.Errorf("without a launcher each pod gets its own srun\n---\n%s", s)
	}
}
