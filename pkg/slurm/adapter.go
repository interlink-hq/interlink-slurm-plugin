package slurm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/containerd/containerd/log"
	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"

	"github.com/intertwin-eu/interlink-slurm-plugin/pkg/batchsystem"
)

// SlurmBatchSystem adapts SidecarHandler to the batchsystem.BatchSystem
// interface. It reuses the exact same code paths as the HTTP handlers, so
// behaviour is identical whichever entry point is used.
type SlurmBatchSystem struct {
	handler *SidecarHandler
}

var _ batchsystem.BatchSystem = (*SlurmBatchSystem)(nil)

func init() {
	batchsystem.RegisterDefault(batchsystem.BatchSystemSLURM, NewSlurmBatchSystem)
}

// NewSlurmBatchSystem is the batchsystem.Constructor for SLURM. config must be
// a SlurmConfig or *SlurmConfig.
func NewSlurmBatchSystem(ctx context.Context, config interface{}) (batchsystem.BatchSystem, error) {
	var cfg SlurmConfig
	switch c := config.(type) {
	case SlurmConfig:
		cfg = c
	case *SlurmConfig:
		if c == nil {
			return nil, fmt.Errorf("slurm: nil *SlurmConfig")
		}
		cfg = *c
	default:
		return nil, fmt.Errorf("slurm: unexpected config type %T, want SlurmConfig or *SlurmConfig", config)
	}

	jids := make(map[string]*JidStruct)
	return &SlurmBatchSystem{
		handler: &SidecarHandler{
			Config: cfg,
			JIDs:   &jids,
			Ctx:    ctx,
		},
	}, nil
}

// Handler exposes the underlying SidecarHandler so callers can still mount
// the HTTP endpoints (SubmitHandler, StopHandler, ...) on an http.ServeMux.
func (s *SlurmBatchSystem) Handler() *SidecarHandler {
	return s.handler
}

// Initialize creates DataRootFolder and reloads persisted job IDs from disk.
func (s *SlurmBatchSystem) Initialize(ctx context.Context) error {
	if err := s.handler.CreateDirectories(); err != nil {
		return fmt.Errorf("slurm: creating directories: %w", err)
	}
	if err := s.handler.LoadJIDs(); err != nil {
		return fmt.Errorf("slurm: loading job IDs: %w", err)
	}
	return nil
}

// Create translates the pod into a SLURM batch script and submits it with sbatch.
func (s *SlurmBatchSystem) Create(ctx context.Context, pod commonIL.RetrievedPodData) (batchsystem.CreateResult, error) {
	res, err := s.handler.submitPod(ctx, pod)
	if err != nil {
		return batchsystem.CreateResult{}, err
	}
	return batchsystem.CreateResult{PodUID: res.PodUID, PodJID: res.PodJID}, nil
}

// Delete cancels the job with scancel and removes its working directories.
func (s *SlurmBatchSystem) Delete(ctx context.Context, pod *v1.Pod) error {
	return s.handler.deletePod(ctx, pod)
}

// Status queries squeue and maps job states to Kubernetes pod statuses.
func (s *SlurmBatchSystem) Status(ctx context.Context, pods []*v1.Pod) ([]commonIL.PodStatus, error) {
	return s.handler.podStatuses(ctx, pods, "")
}

// GetLogs returns job.out followed by the container's run-<name>.out (or
// init-<name>.out for init containers), trimmed according to opts.Tail /
// opts.LimitBytes. Missing files are treated as empty. Follow, Previous and
// Timestamps are not supported and ignored.
func (s *SlurmBatchSystem) GetLogs(ctx context.Context, pod *v1.Pod, containerName string, opts batchsystem.LogOptions) ([]byte, error) {
	if pod == nil {
		return nil, fmt.Errorf("slurm: cannot get logs for a nil pod")
	}
	if opts.Follow || opts.Previous || opts.Timestamps {
		log.G(ctx).Warning("slurm: GetLogs ignores Follow/Previous/Timestamps options")
	}

	dir := s.handler.resolveLogDir(pod.Namespace, string(pod.UID))

	containerOutput, err := readLogFile(dir + "/run-" + containerName + ".out")
	if err != nil {
		return nil, err
	}
	if containerOutput == nil {
		if containerOutput, err = readLogFile(dir + "/init-" + containerName + ".out"); err != nil {
			return nil, err
		}
	}

	jobOutput, err := readLogFile(dir + "/job.out")
	if err != nil {
		return nil, err
	}

	output := append(jobOutput, containerOutput...)
	return []byte(trimLogOutput(output, opts.Tail, opts.LimitBytes)), nil
}

// GetSystemInfo returns the output of `sinfo -s`.
func (s *SlurmBatchSystem) GetSystemInfo(ctx context.Context) (string, error) {
	return s.handler.getSinfoSummary()
}

// readLogFile reads path, returning (nil, nil) when the file does not exist
// so callers can distinguish "missing" from "empty".
func readLogFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("slurm: reading %s: %w", path, err)
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}
