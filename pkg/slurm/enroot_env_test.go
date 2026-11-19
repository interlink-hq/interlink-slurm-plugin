package slurm

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testContainer = "main"

// A value that breaks or changes if a shell parses it.
const envTestSpecial = `a  b 'c' "d" $HOME ${X:-y} $(touch pwned) ` + "`touch pwned`" + ` \ * ;|&`

// enrootEnvfmt is common::envfmt of enroot 4.2.1 (src/common.sh), which enroot runs over
// the container's environment file twice (src/runtime.sh, end of _do_environ and of
// _do_hooks): among others each pass removes one pair of quotes around a value.
const enrootEnvfmt = `enroot_envfmt() {
  sed -i -e 's/^[[:space:]]\+//' \
    -e '/^#\|^$/d' \
    -e '/^[[:alpha:]_][[:alnum:]_]*=/!d' \
    -e '/^ENROOT_/d' \
    -e 's/^\([[:alpha:]_][[:alnum:]_]*\)=[\"\x27]\(.*\)[\"\x27][[:space:]]*$/\1=\2/' \
    "$1"
}
`

// fakeEnroot stands in for enroot: import and create do nothing, and start sources the
// last --conf and writes what its environ() prints to $FAKE_ENROOT_ENV.
const fakeEnroot = `#!/bin/bash
set -euo pipefail
[ "$1" = start ] || exit 0
shift
conf=
while [ $# -gt 0 ]; do
  case "$1" in
  -c|--conf) conf=$2; shift 2 ;;
  --conf=*) conf=${1#*=}; shift ;;
  -e|--env|-m|--mount) shift 2 ;;
  --) shift; break ;;
  -?*) shift ;;
  *) break ;;
  esac
done
[ -n "$conf" ] && source "$conf"
environ > "$FAKE_ENROOT_ENV"
`

func envTestContainer() v1.Container {
	return v1.Container{
		Name:  testContainer,
		Image: busyboxImage,
		Env: []v1.EnvVar{
			{Name: "PLAIN", Value: "hello"},
			{Name: "SPECIAL", Value: envTestSpecial},
			{Name: "EMPTY", Value: ""},
			{Name: "PORT", Value: "1234"},
			{Name: "DQ", Value: `"quoted"`},
			{Name: "SQ", Value: `'single'`},
			{Name: "MIXQ", Value: `'a' and "b"`},
			{Name: "TRAILSP", Value: `"x"   `},
			{Name: "JSONSTR", Value: `"{\"k\": \"v\"}"`},
			{Name: "BSLASH", Value: `\a\`},
			{Name: "DBSLASH", Value: `\\`},
			{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
			{Name: "MULTI", Value: "x\nINJECTED=1"},
			{Name: "bad.name", Value: "v"},
			{Name: "ENROOT_ROOTFS", Value: "/x"},
		},
	}
}

func envTestPodData(container v1.Container) commonIL.RetrievedPodData {
	return commonIL.RetrievedPodData{
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "envpod", Namespace: metav1.NamespaceDefault, UID: "1b2c3d4e-5f60-4711-8899-aabbccddeeff"},
			Spec:       v1.PodSpec{Containers: []v1.Container{container}},
		},
		Containers: []commonIL.RetrievedContainer{{Name: container.Name}},
	}
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

// enrootTestConfig writes the fake enroot and returns a config that uses it.
func enrootTestConfig(t *testing.T) SlurmConfig {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "enroot")
	if err := os.WriteFile(fake, []byte(fakeEnroot), 0o755); err != nil {
		t.Fatal(err)
	}
	return SlurmConfig{ContainerRuntime: enrootRuntime, EnrootPath: fake, EnrootDefaultOptions: []string{"--rw"}, BashPath: "/bin/bash"}
}

// enrootRuntimeCommand assembles an enroot container command as Create.go does.
func enrootRuntimeCommand(config SlurmConfig, data commonIL.RetrievedPodData, container v1.Container, workDir string) []string {
	runtimeCommand := prepareRuntimeCommand(config, container, data.Pod.ObjectMeta)
	runtimeCommand = append(runtimeCommand, prepareEnvs(context.Background(), config, data, container, workDir)...)
	return append(runtimeCommand, "", container.Name+string(data.Pod.UID))
}

// renderEnrootJob writes job.sh for a pod with one enroot container into a new
// directory and returns it.
func renderEnrootJob(t *testing.T, config SlurmConfig, data commonIL.RetrievedPodData, container v1.Container) string {
	t.Helper()
	workDir := t.TempDir()
	pod := data.Pod
	commands := []ContainerCommand{{
		runtimeCommand:   enrootRuntimeCommand(config, data, container, workDir),
		containerName:    container.Name,
		containerCommand: []string{"true"},
		containerImage:   container.Image,
	}}
	if _, err := produceSLURMScript(context.Background(), config, pod, workDir, pod.ObjectMeta, commands, ResourceLimits{CPU: 1, Memory: 1024 * 1024}, false, false, nil); err != nil {
		t.Fatalf("produceSLURMScript() error: %v", err)
	}
	return workDir
}

// runFakeEnroot runs script with bash and the host environment hostEnv, and returns
// the lines the environ() of the --conf given to enroot start printed.
func runFakeEnroot(t *testing.T, dir, script string, hostEnv ...string) []string {
	t.Helper()
	envOut := filepath.Join(t.TempDir(), "environ")
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "FAKE_ENROOT_ENV=" + envOut}, hostEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s failed: %v\n%s", script, err, out)
	}
	got, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("enroot start did not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
}

// The enroot env file is a configuration script whose environ() gives every value
// unchanged once enroot has formatted it; a variable enroot cannot carry is left out
// rather than split.
func TestCreateEnvFileEnroot(t *testing.T) {
	requireBash(t)
	workDir := t.TempDir()
	container := envTestContainer()

	envs, _, err := createEnvFile(context.Background(), SlurmConfig{ContainerRuntime: enrootRuntime}, envTestPodData(container), container, workDir)
	if err != nil {
		t.Fatalf("createEnvFile() error: %v", err)
	}
	envfilePath := workDir + "/main_envfile.properties"
	if want := []string{"--conf", envfilePath}; !slices.Equal(envs, want) {
		t.Errorf("envs = %q, want %q", envs, want)
	}

	out := filepath.Join(workDir, "environ")
	cmd := exec.Command("bash", "-c", enrootEnvfmt+`set -euo pipefail; source "$1"; environ > "$2"; enroot_envfmt "$2"; enroot_envfmt "$2"`, "bash", envfilePath, out)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Dir = workDir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("environ() of the conf script failed: %v\n%s", err, b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "PLAIN=hello\nSPECIAL=" + envTestSpecial + "\nEMPTY=\nPORT=1234\n" +
		"DQ=\"quoted\"\nSQ='single'\nMIXQ='a' and \"b\"\nTRAILSP=\"x\"   \nJSONSTR=\"{\\\"k\\\": \\\"v\\\"}\"\n" +
		"BSLASH=\\a\\\nDBSLASH=\\\\\n" +
		"NVIDIA_VISIBLE_DEVICES=all\n"
	if string(got) != want {
		t.Errorf("environment\n%q\nwant\n%q", got, want)
	}
	if _, err := os.Stat(filepath.Join(workDir, "pwned")); err == nil {
		t.Error("a value was executed by the shell")
	}
}

// The environ() of the --conf that job.sh hands enroot prints the pod's env and nothing of
// the host's. A value that starts with a quote or a backslash gets two pairs of quotes, so
// it is unchanged after enroot's two envfmt passes.
func TestEnrootJobScriptEnviron(t *testing.T) {
	requireBash(t)
	config := enrootTestConfig(t)
	container := v1.Container{
		Name:  testContainer,
		Image: busyboxImage,
		Env: []v1.EnvVar{
			{Name: "PLAIN", Value: "hello"}, {Name: "QUOTED", Value: "'n1:6379'"},
			{Name: "DQ", Value: `"v"`}, {Name: "BS", Value: `\a\`},
		},
	}
	workDir := renderEnrootJob(t, config, envTestPodData(container), container)

	// the host's variables do not reach the container
	got := runFakeEnroot(t, workDir, "exec bash job.sh", "HOST_ONLY=1", "SLURM_PROCID=0")

	want := []string{"PLAIN='hello'", "QUOTED='''n1:6379'''", `DQ=''"v"''`, `BS=''\a\''`}
	if !slices.Equal(got, want) {
		t.Errorf("environ()\n%q\nwant\n%q", got, want)
	}

	environ := filepath.Join(t.TempDir(), "environ")
	if err := os.WriteFile(environ, []byte(strings.Join(got, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", enrootEnvfmt+`set -euo pipefail; enroot_envfmt "$1"; enroot_envfmt "$1"`, "bash", environ)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("enroot_envfmt failed: %v\n%s", err, b)
	}
	formatted, err := os.ReadFile(environ)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"PLAIN=hello", "QUOTED='n1:6379'", `DQ="v"`, `BS=\a\`} {
		if !slices.Contains(strings.Split(string(formatted), "\n"), line) {
			t.Errorf("after two envfmt passes, no line %q in\n%s", line, formatted)
		}
	}
}

// envFrom Secrets and ConfigMaps, with and without a prefix, reach the enroot environment
// as env does.
func TestCreateEnvFileEnrootEnvFrom(t *testing.T) {
	requireBash(t)
	workDir := t.TempDir()
	container := v1.Container{Name: testContainer, EnvFrom: []v1.EnvFromSource{
		{SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "s"}}},
		{Prefix: "CM_", ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "c"}}},
	}}
	data := envTestPodData(container)
	data.Containers[0].Secrets = []v1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "s"}, Data: map[string][]byte{"SECRET_VALUE": []byte("my-secret")}}}
	data.Containers[0].ConfigMaps = []v1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Data: map[string]string{"URL": `'quoted' $HOME`}}}

	envfilePath := workDir + "/main_envfile.properties"
	if _, _, err := createEnvFile(context.Background(), SlurmConfig{ContainerRuntime: enrootRuntime}, data, container, workDir); err != nil {
		t.Fatalf("createEnvFile() error: %v", err)
	}
	out := filepath.Join(workDir, "environ")
	cmd := exec.Command("bash", "-c", enrootEnvfmt+`set -euo pipefail; source "$1"; environ > "$2"; enroot_envfmt "$2"; enroot_envfmt "$2"`, "bash", envfilePath, out)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("environ failed: %v\n%s", err, b)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	slices.Sort(got)
	if want := []string{"CM_URL='quoted' $HOME", "SECRET_VALUE=my-secret"}; !slices.Equal(got, want) {
		t.Errorf("environment %q, want %q", got, want)
	}
}

// As in Kubernetes, env wins over envFrom for a name in both, and among envFrom sources the
// last one wins. Both runtimes keep the last line of a name: singularity evaluates the env
// file as a shell script, and enroot-switchroot sets the lines of the environment file in order.
func TestCreateEnvFileEnvOverridesEnvFrom(t *testing.T) {
	requireBash(t)
	// IN_BOTH is set by env and by both envFrom sources, IN_SOURCES by both sources.
	const inBoth, inSources = "IN_BOTH", "IN_SOURCES"
	container := v1.Container{
		Name: testContainer,
		Env:  []v1.EnvVar{{Name: inBoth, Value: "env"}},
		EnvFrom: []v1.EnvFromSource{
			{SecretRef: &v1.SecretEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "s"}}},
			{ConfigMapRef: &v1.ConfigMapEnvSource{LocalObjectReference: v1.LocalObjectReference{Name: "c"}}},
		},
	}
	data := envTestPodData(container)
	data.Containers[0].Secrets = []v1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "s"}, Data: map[string][]byte{inBoth: []byte("secret"), inSources: []byte("first")}}}
	data.Containers[0].ConfigMaps = []v1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Data: map[string]string{inBoth: "configmap", inSources: "last"}}}

	for _, tc := range []struct{ runtime, script string }{
		{singularityRuntime, `set -euo pipefail; source "$1"; printf 'IN_BOTH=%s\nIN_SOURCES=%s\n' "$IN_BOTH" "$IN_SOURCES" > "$2"`},
		{enrootRuntime, enrootEnvfmt + `set -euo pipefail; source "$1"; environ > "$2"; enroot_envfmt "$2"; enroot_envfmt "$2"`},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			workDir := t.TempDir()
			if _, _, err := createEnvFile(context.Background(), SlurmConfig{ContainerRuntime: tc.runtime}, data, container, workDir); err != nil {
				t.Fatalf("createEnvFile() error: %v", err)
			}
			out := filepath.Join(workDir, "environ")
			cmd := exec.Command("bash", "-c", tc.script, "bash", workDir+"/main_envfile.properties", out) //nolint:gosec // tc.script is one of the scripts above
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("reading the env file failed: %v\n%s", err, b)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
				name, value, _ := strings.Cut(line, "=")
				got[name] = value
			}
			if want := map[string]string{inBoth: "env", inSources: "last"}; !maps.Equal(got, want) {
				t.Errorf("environment %q, want %q", got, want)
			}
		})
	}
}

// A -c or --conf in the enroot options is found, quoted or not, so the plugin can log that
// its own --conf replaces it.
func TestEnrootConfOption(t *testing.T) {
	for _, tc := range []struct{ defaults, annotation, want string }{
		{"--rw", "--mount /a:/a", ""},
		{"--rw --conf /site.sh", "", "--conf"},
		{"", "--rw -c /a.sh", "-c"},
		{"", "--conf=/a.sh", "--conf=/a.sh"},
		{"", `"--conf" /a.sh`, "--conf"},
	} {
		if got := enrootConfOption(tc.defaults, tc.annotation); got != tc.want {
			t.Errorf("enrootConfOption(%q, %q) = %q, want %q", tc.defaults, tc.annotation, got, tc.want)
		}
	}
}

// Singularity keeps its shell-quoted env file and gets no enroot arguments.
func TestPrepareEnvsSingularityUnchanged(t *testing.T) {
	workDir := t.TempDir()
	container := v1.Container{
		Name: testContainer,
		Env: []v1.EnvVar{
			{Name: "SPECIAL", Value: `it's $HOME`},
			{Name: "MULTI", Value: "x\ny"},
			{Name: "COUNT", Value: "0"},
		},
	}

	envs := prepareEnvs(context.Background(), SlurmConfig{ContainerRuntime: singularityRuntime}, envTestPodData(container), container, workDir)
	envfilePath := workDir + "/main_envfile.properties"
	if want := []string{"--env-file", envfilePath}; !slices.Equal(envs, want) {
		t.Errorf("envs = %q, want %q", envs, want)
	}
	got, err := os.ReadFile(envfilePath)
	if err != nil {
		t.Fatal(err)
	}
	want := "SPECIAL='it'\"'\"'s $HOME'\nMULTI='x\ny'\nCOUNT=0\n"
	if string(got) != want {
		t.Errorf("env file\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(strings.Join(prepareRuntimeCommand(SlurmConfig{ContainerRuntime: singularityRuntime}, container, metav1.ObjectMeta{}), " "), "--env") {
		t.Error("singularity command carries --env")
	}
}
