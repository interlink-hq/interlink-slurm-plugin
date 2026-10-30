package slurm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // SiteTimeZone works in a minimal image

	"github.com/containerd/containerd/log"

	exec "github.com/alexellis/go-execute/pkg/v1"
)

// EstimateRequest asks when Slurm would start a job of Nodes nodes with GPUsPerNode
// GPUs each, submitted now with the default flavor's flags and, when set, the CPUs each
// node's task needs (sites that tie CPUs to GPUs refuse a GPU job without them) and the
// job's own time limit (HH:MM:SS), which backfill depends on. Used by a Kueue admission check, so
// that a multi-node job is admitted only when the site can run it soon.
type EstimateRequest struct {
	Nodes       int    `json:"nodes"`
	GPUsPerNode int    `json:"gpusPerNode"`
	CPUsPerNode int    `json:"cpusPerNode,omitempty"`
	Time        string `json:"time,omitempty"`
}

// estimateTimeRe is the only time limit form passed on: the flags reach a shell.
var estimateTimeRe = regexp.MustCompile(`^\d{1,3}:[0-5]\d:[0-5]\d$`)

// EstimateResponse is Slurm's answer: the expected start time, and its raw output.
type EstimateResponse struct {
	Start time.Time `json:"start"`
	Raw   string    `json:"raw"`
}

// testOnlyStartRe reads `sbatch --test-only`'s answer, e.g.
// "sbatch: Job 123 to start at 2026-10-22T20:00:00 using 160 processors on nodes ...".
var testOnlyStartRe = regexp.MustCompile(`to start at (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})`)

// estimateFlags builds the sbatch arguments of a test-only submission.
func estimateFlags(config SlurmConfig, req EstimateRequest) []string {
	flags := []string{"--test-only", "--nodes=" + strconv.Itoa(req.Nodes), "--ntasks-per-node=1"}
	if flavor, ok := config.Flavors[config.DefaultFlavor]; ok {
		flags = append(flags, flavor.SlurmFlags...)
	}
	if req.CPUsPerNode > 0 {
		flags = append(flags, "--cpus-per-task="+strconv.Itoa(req.CPUsPerNode))
	}
	if req.Time != "" {
		// after the flavor's flags: sbatch keeps the last --time
		flags = append(flags, "--time="+req.Time)
	}
	if req.GPUsPerNode > 0 && !hasGPUFlag(flags) {
		switch config.GPUFlag {
		case GPUFlagGPUsPerNode, GPUFlagGPUs:
			n := req.GPUsPerNode
			if config.GPUFlag == GPUFlagGPUs {
				n *= req.Nodes
			}
			flags = append(flags, "--"+config.GPUFlag+"="+strconv.Itoa(n))
		default:
			flags = append(flags, "--gres=gpu:"+strconv.Itoa(req.GPUsPerNode))
		}
	}
	return append(flags, "--wrap=true")
}

func hasGPUFlag(flags []string) bool {
	for _, f := range flags {
		if declaresGPUs(f) {
			return true
		}
	}
	return false
}

// parseTestOnly returns the start time Slurm announced, in the local time zone of the
// site (Slurm prints local time without a zone). In the hour that repeats when summer time
// ends, a local time has two readings: Slurm means the earliest that is not in the past (a
// start estimate never is), where ParseInLocation may pick the later one.
func parseTestOnly(output string, loc *time.Location, now time.Time) (time.Time, error) {
	const layout = "2006-01-02T15:04:05"
	m := testOnlyStartRe.FindStringSubmatch(output)
	if m == nil {
		return time.Time{}, fmt.Errorf("no start time in sbatch --test-only output: %q", strings.TrimSpace(output))
	}
	parsed, err := time.ParseInLocation(layout, m[1], loc)
	if err != nil {
		return parsed, err
	}
	start := parsed
	for _, c := range []time.Time{parsed.Add(-time.Hour), parsed, parsed.Add(time.Hour)} {
		if c.In(loc).Format(layout) != m[1] || c.Before(now.Add(-time.Minute)) {
			continue
		}
		if start.Before(now.Add(-time.Minute)) || c.Before(start) {
			start = c
		}
	}
	return start, nil
}

// EstimateHandler answers POST /estimate with when Slurm would start such a job now.
func (h *SidecarHandler) EstimateHandler(w http.ResponseWriter, r *http.Request) {
	var req EstimateRequest
	body, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err == nil && req.Time != "" && !estimateTimeRe.MatchString(req.Time) {
		err = fmt.Errorf("time %q is not HH:MM:SS", req.Time)
	}
	if err != nil || req.Nodes < 1 {
		http.Error(w, fmt.Sprintf("want {\"nodes\": N >= 1, \"gpusPerNode\": M, \"time\": \"HH:MM:SS\"}: %v", err), http.StatusBadRequest)
		return
	}
	task := exec.ExecTask{Command: h.Config.Sbatchpath, Args: estimateFlags(h.Config, req), Shell: true}
	res, err := task.Execute()
	out := res.Stdout + res.Stderr
	if err != nil {
		http.Error(w, fmt.Sprintf("sbatch --test-only: %v: %s", err, out), http.StatusBadGateway)
		return
	}
	loc := time.Local
	if h.Config.SiteTimeZone != "" {
		if loc, err = time.LoadLocation(h.Config.SiteTimeZone); err != nil {
			http.Error(w, fmt.Sprintf("SiteTimeZone: %v", err), http.StatusInternalServerError)
			return
		}
	}
	start, err := parseTestOnly(out, loc, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	log.G(h.Ctx).Infof("Estimate for %d node(s) x %d GPU(s): start at %s", req.Nodes, req.GPUsPerNode, start)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(EstimateResponse{Start: start, Raw: strings.TrimSpace(out)}); err != nil {
		log.G(h.Ctx).Error(err)
	}
}
