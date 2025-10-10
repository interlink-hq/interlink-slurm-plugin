package slurm

import (
	"context"
	"fmt"

	"github.com/intertwin-eu/interlink-slurm-plugin/pkg/batchsystem"
	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
)

// SlurmBatchSystem is an adapter that wraps the existing SidecarHandler to implement
// the batchsystem.BatchSystem interface. This allows the SLURM plugin to work with
// the new SDK-like architecture while maintaining backward compatibility.
type SlurmBatchSystem struct {
	handler *SidecarHandler
}

// NewSlurmBatchSystem creates a new SLURM batch system instance from a configuration.
// This is the constructor function that will be registered with the factory.
func NewSlurmBatchSystem(ctx context.Context, config interface{}) (batchsystem.BatchSystem, error) {
	slurmConfig, ok := config.(SlurmConfig)
	if !ok {
		return nil, fmt.Errorf("invalid config type for SLURM batch system: expected SlurmConfig")
	}

	jobIDs := make(map[string]*JidStruct)

	handler := &SidecarHandler{
		Config: slurmConfig,
		JIDs:   &jobIDs,
		Ctx:    ctx,
	}

	return &SlurmBatchSystem{
		handler: handler,
	}, nil
}

// Initialize performs setup for the SLURM batch system, including creating
// necessary directories and loading saved job state.
func (s *SlurmBatchSystem) Initialize(ctx context.Context) error {
	s.handler.CreateDirectories()
	s.handler.LoadJIDs()
	return nil
}

// Create submits a new pod to SLURM and returns the job identifier.
// This method wraps the existing SLURM creation logic to conform to the interface.
func (s *SlurmBatchSystem) Create(ctx context.Context, pod commonIL.RetrievedPodData) (batchsystem.CreateResult, error) {
	containers := pod.Pod.Spec.InitContainers
	containers = append(containers, pod.Pod.Spec.Containers...)
	metadata := pod.Pod.ObjectMeta
	filesPath := s.handler.Config.DataRootFolder + pod.Pod.Namespace + "-" + string(pod.Pod.UID)

	// This is a simplified version - in production, you'd want to extract all the logic
	// from SubmitHandler into a reusable method
	var runtime_command_pod []ContainerCommand
	var resourceLimits ResourceLimits

	// Process containers (simplified - the full logic should be extracted from SubmitHandler)
	for i, container := range containers {
		mounts, err := prepareMounts(ctx, s.handler.Config, &pod, &container, filesPath)
		if err != nil {
			return batchsystem.CreateResult{}, fmt.Errorf("failed to prepare mounts: %w", err)
		}

		envs := prepareEnvs(ctx, s.handler.Config, pod, container)
		image := prepareImage(ctx, s.handler.Config, metadata, container.Image)
		commstr1 := prepareRuntimeCommand(s.handler.Config, container, metadata)
		runtime_command := append(commstr1, envs...)

		switch s.handler.Config.ContainerRuntime {
		case "singularity":
			runtime_command = append(runtime_command, mounts)
			runtime_command = append(runtime_command, image)
		case "enroot":
			// Enroot-specific logic
			runtime_command = append(runtime_command, mounts)
		}

		isInit := i < len(pod.Pod.Spec.InitContainers)

		// Process probes if enabled
		var readinessProbes, livenessProbes, startupProbes []ProbeCommand
		if s.handler.Config.EnableProbes && !isInit {
			readinessProbes, livenessProbes, startupProbes = translateKubernetesProbes(ctx, container)
		}

		runtime_command_pod = append(runtime_command_pod, ContainerCommand{
			runtimeCommand:   runtime_command,
			containerName:    container.Name,
			containerArgs:    container.Args,
			containerCommand: container.Command,
			isInitContainer:  isInit,
			readinessProbes:  readinessProbes,
			livenessProbes:   livenessProbes,
			startupProbes:    startupProbes,
			containerImage:   image,
		})
	}

	// Generate SLURM script
	path, err := produceSLURMScript(ctx, s.handler.Config, pod.Pod, filesPath, metadata, runtime_command_pod, resourceLimits, true, true)
	if err != nil {
		return batchsystem.CreateResult{}, fmt.Errorf("failed to produce SLURM script: %w", err)
	}

	// Submit to SLURM
	out, err := SLURMBatchSubmit(s.handler.Ctx, s.handler.Config, path)
	if err != nil {
		return batchsystem.CreateResult{}, fmt.Errorf("failed to submit SLURM job: %w", err)
	}

	jid, err := handleJidAndPodUid(s.handler.Ctx, pod.Pod, s.handler.JIDs, out, filesPath)
	if err != nil {
		return batchsystem.CreateResult{}, fmt.Errorf("failed to handle job ID: %w", err)
	}

	return batchsystem.CreateResult{
		PodUID: string(pod.Pod.UID),
		PodJID: jid,
	}, nil
}

// Delete cancels a SLURM job and cleans up associated resources.
func (s *SlurmBatchSystem) Delete(ctx context.Context, pod *v1.Pod) error {
	filesPath := s.handler.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID)
	return deleteContainer(ctx, s.handler.Config, string(pod.UID), s.handler.JIDs, filesPath)
}

// Status retrieves the current status of pods from SLURM.
func (s *SlurmBatchSystem) Status(ctx context.Context, pods []*v1.Pod) ([]commonIL.PodStatus, error) {
	// The status logic is complex and tightly coupled with the StatusHandler.
	// In a full refactoring, this would be extracted into a separate method.
	// For now, we note that this would call the same underlying SLURM status checking logic.
	return nil, fmt.Errorf("status check should use StatusHandler directly for now")
}

// GetLogs retrieves logs for a specific container in a pod.
func (s *SlurmBatchSystem) GetLogs(ctx context.Context, pod *v1.Pod, containerName string, opts batchsystem.LogOptions) ([]byte, error) {
	path := s.handler.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID)
	containerOutputPath := path + "/run-" + containerName + ".out"

	// Read the logs (simplified version)
	output, err := s.handler.ReadLogs(containerOutputPath, nil, ctx, nil, "")
	if err != nil {
		return nil, fmt.Errorf("failed to read container logs: %w", err)
	}

	return output, nil
}

// GetSystemInfo returns SLURM cluster information using 'sinfo -s'.
func (s *SlurmBatchSystem) GetSystemInfo(ctx context.Context) (string, error) {
	return s.handler.getSinfoSummary()
}

// Ensure SlurmBatchSystem implements the BatchSystem interface
var _ batchsystem.BatchSystem = (*SlurmBatchSystem)(nil)

// init registers the SLURM batch system with the default factory
func init() {
	batchsystem.RegisterDefault(batchsystem.BatchSystemSLURM, NewSlurmBatchSystem)
}
