package slurm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testFlavor is the default flavor's name.
const testFlavor = "default"

func TestEstimateFlags(t *testing.T) {
	config := SlurmConfig{
		DefaultFlavor: testFlavor,
		Flavors:       map[string]FlavorConfig{testFlavor: {Name: testFlavor, SlurmFlags: []string{"--qos=debug", "-A", "proj"}}},
	}
	got := strings.Join(estimateFlags(config, EstimateRequest{Nodes: 2, GPUsPerNode: 4}), " ")
	for _, want := range []string{"--test-only", "--nodes=2", "--qos=debug", "-A proj", testGres4, "--wrap=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("estimate flags %q missing %q", got, want)
		}
	}
	config.Flavors[testFlavor] = FlavorConfig{Name: testFlavor, SlurmFlags: []string{"--time=00:30:00"}}
	got = strings.Join(estimateFlags(config, EstimateRequest{Nodes: 1, GPUsPerNode: 1, CPUsPerNode: 20}), " ")
	if !strings.Contains(got, "--ntasks-per-node=1") || !strings.Contains(got, "--cpus-per-task=20") {
		t.Errorf("CPUsPerNode: want one task of 20 CPUs per node in %q", got)
	}
	got = strings.Join(estimateFlags(config, EstimateRequest{Nodes: 2, GPUsPerNode: 4, Time: "01:45:00"}), " ")
	if i, j := strings.Index(got, "--time=00:30:00"), strings.Index(got, "--time=01:45:00"); j < 0 || j < i {
		t.Errorf("the job's time limit must follow the flavor's, got %q", got)
	}
	config.GPUFlag = GPUFlagGPUs
	if got := strings.Join(estimateFlags(config, EstimateRequest{Nodes: 2, GPUsPerNode: 4}), " "); !strings.Contains(got, "--gpus=8") {
		t.Errorf("GPUFlag gpus: want the total --gpus=8 in %q", got)
	}
}

func TestParseTestOnlyRepeatedHour(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 01:30 happens twice on 2026-11-01 in New York: 05:30Z (summer time) and 06:30Z
	out := "sbatch: Job 1001 to start at 2026-11-01T01:30:00 using 320 processors on nodes n[01-02] in partition gpu"
	for _, c := range []struct {
		now, want time.Time
	}{
		{time.Date(2026, 11, 1, 5, 20, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC), time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)},
	} {
		got, err := parseTestOnly(out, loc, c.now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("now %s: got %s (%v), want %s", c.now, got.UTC(), err, c.want)
		}
	}
	// east of UTC ParseInLocation takes the later reading: 02:14:39 on 2026-10-25 in Paris is
	// 00:14:39Z (summer time) and 01:14:39Z
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	out = "sbatch: Job 1003 to start at 2026-10-25T02:14:39 using 320 processors on nodes n[01-02] in partition gpu"
	got, err := parseTestOnly(out, paris, time.Date(2026, 10, 25, 0, 14, 0, 0, time.UTC))
	if want := time.Date(2026, 10, 25, 0, 14, 39, 0, time.UTC); err != nil || !got.Equal(want) {
		t.Errorf("Paris: got %s (%v), want %s", got.UTC(), err, want)
	}
	// an ordinary hour has one reading
	out = "sbatch: Job 1002 to start at 2026-10-24T22:00:00 using 160 processors on nodes n01 in partition gpu"
	got, err = parseTestOnly(out, loc, time.Date(2026, 10, 25, 1, 59, 0, 0, time.UTC))
	if want := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC); err != nil || !got.Equal(want) {
		t.Errorf("got %s (%v), want %s", got.UTC(), err, want)
	}
}

func TestEstimateHandler(t *testing.T) {
	sbatch := filepath.Join(t.TempDir(), "sbatch")
	script := "#!/bin/sh\necho 'sbatch: Job 4242 to start at 2026-10-22T20:15:00 using 160 processors on nodes n[01-02] in partition gpu' >&2\n"
	if err := os.WriteFile(sbatch, []byte(script), 0o700); err != nil { //nolint:gosec // a fake executable
		t.Fatal(err)
	}
	h := &SidecarHandler{Config: SlurmConfig{Sbatchpath: sbatch}, Ctx: context.Background()}

	rec := httptest.NewRecorder()
	h.EstimateHandler(rec, httptest.NewRequest(http.MethodPost, "/estimate", bytes.NewReader([]byte(`{"nodes":2,"gpusPerNode":4}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
	}
	var resp EstimateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 22, 20, 15, 0, 0, time.Local)
	if !resp.Start.Equal(want) {
		t.Errorf("start = %s, want %s", resp.Start, want)
	}

	rec = httptest.NewRecorder()
	h.EstimateHandler(rec, httptest.NewRequest(http.MethodPost, "/estimate", bytes.NewReader([]byte(`{"nodes":0}`))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("nodes 0: code %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.EstimateHandler(rec, httptest.NewRequest(http.MethodPost, "/estimate", bytes.NewReader([]byte(`{"nodes":1,"time":"1:00:00; rm -rf ~"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("time not HH:MM:SS: code %d, want 400", rec.Code)
	}
}
