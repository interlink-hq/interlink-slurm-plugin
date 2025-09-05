package slurm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/log"
	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
)

// processConfigMapVolume handles ConfigMap volume mounting
func processConfigMapVolume(
	ctx context.Context,
	config Config,
	container *v1.Container,
	workingPath string,
	volume v1.Volume,
	volumeMount v1.VolumeMount,
	podData *commonIL.RetrievedPodData,
	mountedDataSB *strings.Builder,
) error {
	retrievedContainer, err := getRetrievedContainer(podData, container.Name)
	if err != nil {
		return err
	}

	retrievedConfigMap, err := getRetrievedConfigMap(retrievedContainer, volume.ConfigMap.Name, container.Name, podData.Pod.Name)
	if err != nil {
		return err
	}

	return prepareMountsSimpleVolume(ctx, config, container, workingPath, *retrievedConfigMap, volumeMount, volume, mountedDataSB)
}

// processProjectedVolume handles Projected volume mounting
func processProjectedVolume(
	ctx context.Context,
	config Config,
	container *v1.Container,
	workingPath string,
	volume v1.Volume,
	volumeMount v1.VolumeMount,
	podData *commonIL.RetrievedPodData,
	mountedDataSB *strings.Builder,
) error {
	retrievedContainer, err := getRetrievedContainer(podData, container.Name)
	if err != nil {
		return err
	}

	retrievedProjectedVolumeMap, err := getRetrievedProjectedVolumeMap(retrievedContainer, volume.Name, container.Name, podData.Pod.Name)
	if err != nil {
		return err
	}

	if retrievedProjectedVolumeMap == nil {
		// This should not happen, either this is an error or the flag DisableProjectedVolumes is true in VK. Building context for log.
		var retrievedProjectedVolumeMapKeys []string
		for _, retrievedProjectedVolumeMap := range retrievedContainer.ProjectedVolumeMaps {
			retrievedProjectedVolumeMapKeys = append(retrievedProjectedVolumeMapKeys, retrievedProjectedVolumeMap.Name)
		}
		log.G(ctx).Warningf("projected volumes not found %s in container %s in pod %s, current projectedVolumeMaps keys %s ."+
			"either this is an error or this is because InterLink VK has DisableProjectedVolumes set to true.",
			volume.Name, container.Name, podData.Pod.Name, strings.Join(retrievedProjectedVolumeMapKeys, ","))
		return nil
	}

	return prepareMountsSimpleVolume(ctx, config, container, workingPath, *retrievedProjectedVolumeMap, volumeMount, volume, mountedDataSB)
}

// processSecretVolume handles Secret volume mounting
func processSecretVolume(
	ctx context.Context,
	config Config,
	container *v1.Container,
	workingPath string,
	volume v1.Volume,
	volumeMount v1.VolumeMount,
	podData *commonIL.RetrievedPodData,
	mountedDataSB *strings.Builder,
) error {
	retrievedContainer, err := getRetrievedContainer(podData, container.Name)
	if err != nil {
		return err
	}

	retrievedSecret, err := getRetrievedSecret(retrievedContainer, volume.Secret.SecretName, container.Name, podData.Pod.Name)
	if err != nil {
		return err
	}

	return prepareMountsSimpleVolume(ctx, config, container, workingPath, *retrievedSecret, volumeMount, volume, mountedDataSB)
}

// processEmptyDirVolume handles EmptyDir volume mounting
func processEmptyDirVolume(
	ctx context.Context,
	config Config,
	container *v1.Container,
	workingPath string,
	volume v1.Volume,
	volumeMount v1.VolumeMount,
	mountedDataSB *strings.Builder,
) error {
	// retrievedContainer.EmptyDirs is deprecated in favor of each plugin giving its own emptyDir path, that will be built in mountData().
	edPath, _, err := mountData(ctx, config, container, "emptyDir", volumeMount, volume, workingPath)
	if err != nil {
		log.G(ctx).Error(err)
		return err
	}

	log.G(ctx).Debug("edPath: ", edPath)

	for _, mntData := range edPath {
		mountedDataSB.WriteString(mntData)
	}

	return nil
}

// processHostPathVolume handles HostPath volume mounting
func processHostPathVolume(
	ctx context.Context,
	volume v1.Volume,
	volumeMount v1.VolumeMount,
	podName string,
	mountedDataSB *strings.Builder,
) error {
	log.G(ctx).Info("Handling hostPath volume: ", volume.Name)

	// For hostPath volumes, we just need to bind mount the host path to the container path.
	hostPath := volume.HostPath.Path
	containerPath := volumeMount.MountPath

	if hostPath == "" || containerPath == "" {
		err := fmt.Errorf("hostPath or containerPath is empty for volume %s in pod %s", volume.Name, podName)
		log.G(ctx).Error(err)
		return err
	}

	if volume.Name != volumeMount.Name {
		log.G(ctx).Warningf("Volume name %s does not match volumeMount name %s in pod %s", volume.Name, volumeMount.Name, podName)
		return nil // Skip this volume
	}

	if err := validateAndPrepareHostPath(ctx, volume, podName); err != nil {
		return err
	}

	mountedDataSB.WriteString(" --bind ")
	mountedDataSB.WriteString(hostPath + ":" + containerPath)

	// if the read-only flag is set, we add it to the mountedDataSB
	if volumeMount.ReadOnly {
		mountedDataSB.WriteString(":ro")
	}

	return nil
}

// validateAndPrepareHostPath validates and prepares the host path based on its type
func validateAndPrepareHostPath(ctx context.Context, volume v1.Volume, podName string) error {
	hostPath := volume.HostPath.Path

	switch {
	case volume.HostPath.Type != nil && *volume.HostPath.Type == v1.HostPathDirectory:
		if _, err := os.Stat(hostPath); os.IsNotExist(err) {
			err := fmt.Errorf("hostPath directory %s does not exist for volume %s in pod %s", hostPath, volume.Name, podName)
			log.G(ctx).Error(err)
			return err
		}
	case volume.HostPath.Type != nil && *volume.HostPath.Type == v1.HostPathDirectoryOrCreate:
		if _, err := os.Stat(hostPath); os.IsNotExist(err) {
			err = os.MkdirAll(hostPath, os.ModePerm)
			if err != nil {
				log.G(ctx).Error(err)
				return err
			}
		}
	case volume.HostPath.Type != nil:
		err := fmt.Errorf("unsupported hostPath type %s for volume %s in pod %s", *volume.HostPath.Type, volume.Name, podName)
		log.G(ctx).Error(err)
		return err
	}

	return nil
}

// processVolumeByType handles volume processing by delegating to appropriate handler
func (h *SidecarHandler) processVolumeByType(
	ctx context.Context,
	config Config,
	container *v1.Container,
	workingPath string,
	volume v1.Volume,
	volumeMount v1.VolumeMount,
	podData *commonIL.RetrievedPodData,
	mountedDataSB *strings.Builder,
) error {
	switch {
	case volume.ConfigMap != nil:
		return processConfigMapVolume(ctx, config, container, workingPath, volume, volumeMount, podData, mountedDataSB)
	case volume.Projected != nil:
		return processProjectedVolume(ctx, config, container, workingPath, volume, volumeMount, podData, mountedDataSB)
	case volume.Secret != nil:
		return processSecretVolume(ctx, config, container, workingPath, volume, volumeMount, podData, mountedDataSB)
	case volume.EmptyDir != nil:
		return processEmptyDirVolume(ctx, config, container, workingPath, volume, volumeMount, mountedDataSB)
	case volume.HostPath != nil:
		return processHostPathVolume(ctx, volume, volumeMount, podData.Pod.Name, mountedDataSB)
	default:
		log.G(ctx).Warningf("Silently ignoring unknown volume type of volume: %s in pod %s", volume.Name, podData.Pod.Name)
		return nil
	}
}
