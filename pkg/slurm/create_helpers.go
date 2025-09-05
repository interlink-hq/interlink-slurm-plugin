package slurm

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/containerd/containerd/log"
	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	singularityRunCommand = "run"
)

// ProbeTypeExec already exists as a constant

// processSingleContainer processes a single container and returns the SingularityCommand
func (h *SidecarHandler) processSingleContainer(
	ctx context.Context,
	span trace.Span,
	container v1.Container,
	containerIndex int,
	metadata metav1.ObjectMeta,
	data commonIL.RetrievedPodData,
	filesPath string,
	resourceLimits *ResourceLimits,
	maxCPULimit *int,
	maxMemoryLimit *int,
	cpuLimit *int64,
	memoryLimit *int64,
	isDefaultCPU *bool,
	isDefaultRAM *bool,
) (*SingularityCommand, error) {
	log.G(h.Ctx).Info("- Beginning script generation for container " + container.Name)

	// Extract singularity configuration from annotations
	singularityMounts := ""
	if singMounts, ok := metadata.Annotations["slurm-job.vk.io/singularity-mounts"]; ok {
		singularityMounts = singMounts
	}

	singularityOptions := ""
	if singOpts, ok := metadata.Annotations["slurm-job.vk.io/singularity-options"]; ok {
		singularityOptions = singOpts
	}

	// Determine singularity command type
	singularityCommand := singularityRunCommand
	if len(container.Command) != 0 {
		singularityCommand = string(ProbeTypeExec)
	}

	// Build singularity command
	singularityCmd := []string{h.Config.SingularityPath, singularityCommand}
	singularityCmd = append(singularityCmd, h.Config.SingularityDefaultOptions...)
	singularityCmd = append(singularityCmd, singularityMounts, singularityOptions)

	// Process resource limits
	if err := h.processContainerResourceLimits(container, resourceLimits, maxCPULimit, maxMemoryLimit, cpuLimit, memoryLimit, isDefaultCPU, isDefaultRAM); err != nil {
		return nil, err
	}

	// Prepare mounts
	mounts, err := h.prepareMounts(ctx, h.Config, &data, &container, filesPath)
	log.G(h.Ctx).Debug(mounts)
	if err != nil {
		os.RemoveAll(filesPath)
		return nil, err
	}

	// Prepare environment variables
	envs := prepareEnvs(ctx, h.Config, data, container)

	// Process image and prefix
	image := h.processImagePrefix(container.Image, metadata)

	log.G(h.Ctx).Debug("-- Appending all commands together...")
	singularityCmd = append(singularityCmd, envs...)
	singularityCmd = append(singularityCmd, mounts)
	singularityCmd = append(singularityCmd, image)

	// Determine if this is an init container
	isInit := containerIndex < len(data.Pod.Spec.InitContainers)

	// Set span attributes
	span.SetAttributes(
		attribute.String("job.container"+strconv.Itoa(containerIndex)+".name", container.Name),
		attribute.Bool("job.container"+strconv.Itoa(containerIndex)+".isinit", isInit),
		attribute.StringSlice("job.container"+strconv.Itoa(containerIndex)+".envs", envs),
		attribute.String("job.container"+strconv.Itoa(containerIndex)+".image", image),
		attribute.StringSlice("job.container"+strconv.Itoa(containerIndex)+".command", container.Command),
		attribute.StringSlice("job.container"+strconv.Itoa(containerIndex)+".args", container.Args),
	)

	// Process probes if enabled
	var readinessProbes, livenessProbes []ProbeCommand
	if h.Config.EnableProbes && !isInit {
		readinessProbes, livenessProbes = translateKubernetesProbes(ctx, container)
		if len(readinessProbes) > 0 || len(livenessProbes) > 0 {
			log.G(h.Ctx).Info("-- Container " + container.Name + " has probes configured")
			span.SetAttributes(
				attribute.Int("job.container"+strconv.Itoa(containerIndex)+".readiness_probes", len(readinessProbes)),
				attribute.Int("job.container"+strconv.Itoa(containerIndex)+".liveness_probes", len(livenessProbes)),
			)
		}
	}

	return &SingularityCommand{
		singularityCommand: singularityCmd,
		containerName:      container.Name,
		containerArgs:      container.Args,
		containerCommand:   container.Command,
		isInitContainer:    isInit,
		readinessProbes:    readinessProbes,
		livenessProbes:     livenessProbes,
	}, nil
}

// processContainerResourceLimits processes resource limits for a single container
func (h *SidecarHandler) processContainerResourceLimits(
	container v1.Container,
	resourceLimits *ResourceLimits,
	maxCPULimit *int,
	maxMemoryLimit *int,
	cpuLimit *int64,
	memoryLimit *int64,
	isDefaultCPU *bool,
	isDefaultRAM *bool,
) error {
	cpuLimitFloat := container.Resources.Limits.Cpu().AsApproximateFloat64()
	memoryLimitFromContainer, _ := container.Resources.Limits.Memory().AsInt64()
	cpuLimitFromContainer := int64(math.Ceil(cpuLimitFloat))

	// Process CPU limits
	if cpuLimitFromContainer == 0 && *isDefaultCPU {
		log.G(h.Ctx).Warning(errors.New("Max CPU resource not set for " + container.Name + ". Only 1 CPU will be used"))
		resourceLimits.CPU = 1
	} else if cpuLimitFromContainer > resourceLimits.CPU && *maxCPULimit < int(cpuLimitFromContainer) {
		log.G(h.Ctx).Info("Setting CPU limit to " + strconv.FormatInt(cpuLimitFromContainer, 10))
		*cpuLimit = cpuLimitFromContainer
		*maxCPULimit = int(cpuLimitFromContainer)
		*isDefaultCPU = false
	}

	// Process Memory limits
	if memoryLimitFromContainer == 0 && *isDefaultRAM {
		log.G(h.Ctx).Warning(errors.New("Max Memory resource not set for " + container.Name + ". Only 1MB will be used"))
		resourceLimits.Memory = 1024 * 1024
	} else if memoryLimitFromContainer > resourceLimits.Memory && *maxMemoryLimit < int(memoryLimitFromContainer) {
		log.G(h.Ctx).Info("Setting Memory limit to " + strconv.FormatInt(memoryLimitFromContainer, 10))
		*memoryLimit = memoryLimitFromContainer
		*maxMemoryLimit = int(memoryLimitFromContainer)
		*isDefaultRAM = false
	}

	resourceLimits.CPU = *cpuLimit
	resourceLimits.Memory = *memoryLimit

	return nil
}

// processImagePrefix processes the container image with prefix handling
func (h *SidecarHandler) processImagePrefix(containerImage string, metadata metav1.ObjectMeta) string {
	image := containerImage
	imagePrefix := h.Config.ImagePrefix

	imagePrefixAnnotationFound := false
	if imagePrefixAnnotation, ok := metadata.Annotations["slurm-job.vk.io/image-root"]; ok {
		// This takes precedence over ImagePrefix
		imagePrefix = imagePrefixAnnotation
		imagePrefixAnnotationFound = true
	}
	log.G(h.Ctx).Info("imagePrefix from annotation? ", imagePrefixAnnotationFound, " value: ", imagePrefix)

	// If imagePrefix begins with "/", then it must be an absolute path instead of for example docker://some/image.
	// The file should be one of https://docs.sylabs.io/guides/3.1/user-guide/cli/singularity_run.html#synopsis format.
	switch {
	case strings.HasPrefix(image, "/"):
		log.G(h.Ctx).Warningf("image set to %s is an absolute path. Prefix won't be added.", image)
	case !strings.HasPrefix(image, imagePrefix):
		image = imagePrefix + containerImage
	default:
		log.G(h.Ctx).Warningf("imagePrefix set to %s but already present in the image name %s. Prefix won't be added.", imagePrefix, image)
	}

	return image
}
