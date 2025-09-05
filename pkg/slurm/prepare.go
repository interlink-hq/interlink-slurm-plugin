package slurm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	exec2 "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"

	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

type SidecarHandler struct {
	Config Config
	JIDs   *map[string]*JidStruct
	Ctx    context.Context
}

var (
	timer        time.Time
	cachedStatus []commonIL.PodStatus
)

type JidStruct struct {
	PodUID       string    `json:"PodUID"`
	PodNamespace string    `json:"PodNamespace"`
	JID          string    `json:"JID"`
	StartTime    time.Time `json:"StartTime"`
	EndTime      time.Time `json:"EndTime"`
}

type ResourceLimits struct {
	CPU    int64
	Memory int64
}

type SingularityCommand struct {
	containerName      string
	isInitContainer    bool
	singularityCommand []string
	containerCommand   []string
	containerArgs      []string
	readinessProbes    []ProbeCommand
	livenessProbes     []ProbeCommand
}

// stringToHex encodes the provided str string into a hex string and removes all trailing redundant zeroes to keep the output more compact
func stringToHex(str string) string {
	var buffer bytes.Buffer
	for _, char := range str {
		err := binary.Write(&buffer, binary.LittleEndian, char)
		if err != nil {
			fmt.Println("Error converting character:", err)
			return ""
		}
	}

	hexString := hex.EncodeToString(buffer.Bytes())
	hexBytes := []byte(hexString)
	var hexReturn string
	for i := 0; i < len(hexBytes); i += 2 {
		if hexBytes[i] != 48 && hexBytes[i+1] != 48 {
			hexReturn += string(hexBytes[i]) + string(hexBytes[i+1])
		}
	}
	return hexReturn
}

// parsingTimeFromString parses time from a string and returns it into a variable of type time.Time.
// The format time can be specified in the 3rd argument.
func parsingTimeFromString(ctx context.Context, stringTime string, timestampFormat string) (time.Time, error) {
	parts := strings.Fields(stringTime)
	if len(parts) != 4 {
		err := errors.New("invalid timestamp format")
		log.G(ctx).Error(err)
		return time.Time{}, err
	}

	parsedTime, err := time.Parse(timestampFormat, stringTime)
	if err != nil {
		log.G(ctx).Error(err)
		return time.Time{}, err
	}

	return parsedTime, nil
}

// CreateDirectories is just a function to be sure directories exists at runtime
func (h *SidecarHandler) CreateDirectories() error {
	path := h.Config.DataRootFolder
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			err = os.MkdirAll(path, os.ModePerm)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadJIDs loads Job IDs into the main JIDs struct from files in the root folder.
// It's useful went down and needed to be restarded, but there were jobs running, for example.
// Return only error in case of failure
func (h *SidecarHandler) LoadJIDs() error {
	path := h.Config.DataRootFolder

	dir, err := os.Open(path)
	if err != nil {
		log.G(h.Ctx).Error(err)
		return err
	}
	defer dir.Close()

	entries, err := dir.ReadDir(0)
	if err != nil {
		log.G(h.Ctx).Error(err)
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			var podNamespace []byte
			var podUID []byte
			StartedAt := time.Time{}
			FinishedAt := time.Time{}

			JID, err := os.ReadFile(path + entry.Name() + "/" + "JobID.jid")
			if err != nil {
				log.G(h.Ctx).Debug(err)
				continue
			}
			podUID, err = os.ReadFile(path + entry.Name() + "/" + "PodUID.uid")
			if err != nil {
				log.G(h.Ctx).Debug(err)
				continue
			}
			podNamespace, err = os.ReadFile(path + entry.Name() + "/" + "PodNamespace.ns")
			if err != nil {
				log.G(h.Ctx).Debug(err)
				continue
			}

			StartedAtString, err := os.ReadFile(path + entry.Name() + "/" + "StartedAt.time")
			if err != nil {
				log.G(h.Ctx).Debug(err)
			} else {
				StartedAt, err = parsingTimeFromString(h.Ctx, string(StartedAtString), "2006-01-02 15:04:05.999999999 -0700 MST")
				if err != nil {
					log.G(h.Ctx).Debug(err)
				}
			}

			FinishedAtString, err := os.ReadFile(path + entry.Name() + "/" + "FinishedAt.time")
			if err != nil {
				log.G(h.Ctx).Debug(err)
			} else {
				FinishedAt, err = parsingTimeFromString(h.Ctx, string(FinishedAtString), "2006-01-02 15:04:05.999999999 -0700 MST")
				if err != nil {
					log.G(h.Ctx).Debug(err)
				}
			}
			JIDEntry := JidStruct{PodUID: string(podUID), PodNamespace: string(podNamespace), JID: string(JID), StartTime: StartedAt, EndTime: FinishedAt}
			(*h.JIDs)[string(podUID)] = &JIDEntry
		}
	}

	return nil
}

func createEnvFile(ctx context.Context, config Config, podData commonIL.RetrievedPodData, container v1.Container) ([]string, []string, error) {
	envs := []string{}
	// For debugging purpose only
	envsData := []string{}

	envfilePath := (config.DataRootFolder + podData.Pod.Namespace + "-" + string(podData.Pod.UID) + "/" + container.Name + "_envfile.properties")
	log.G(ctx).Info("-- Appending envs using envfile " + envfilePath)
	envs = append(envs, "--env-file")
	envs = append(envs, envfilePath)

	envfile, err := os.Create(envfilePath)
	if err != nil {
		log.G(ctx).Error(err)
		return nil, nil, err
	}
	defer envfile.Close()

	for _, envVar := range container.Env {
		// The environment variable values can contains all sort of simple/double quote and space and any arbitrary values.
		// singularity reads the env-file and parse it like a shell string, so shellescape will escape any quote properly.
		tmpValue := shellescape.Quote(envVar.Value)
		tmp := (envVar.Name + "=" + tmpValue)

		envsData = append(envsData, tmp)

		_, err := envfile.WriteString(tmp + "\n")
		if err != nil {
			log.G(ctx).Error(err)
			return nil, nil, err
		}
		log.G(ctx).Debug("---- Written envfile file " + envfilePath + " key " + envVar.Name + " value " + tmpValue)
	}

	// All env variables are written, we flush it now.
	err = envfile.Sync()
	if err != nil {
		log.G(ctx).Error(err)
		return nil, nil, err
	}

	// Calling Close() in case of error. If not error, the defer will close it again but it should be idempotent.
	envfile.Close()

	return envs, envsData, nil
}

// prepareEnvs reads all Environment variables from a container and append them to a envfile.properties. The values are sh-escaped.
// It returns the slice containing, if there are Environment variables, the arguments for envfile and its path, or else an empty array.
func prepareEnvs(ctx context.Context, config Config, podData commonIL.RetrievedPodData, container v1.Container) []string {
	start := time.Now().UnixMicro()
	span := trace.SpanFromContext(ctx)
	span.AddEvent("Preparing ENVs for container " + container.Name)
	var envs = []string{}
	// For debugging purpose only
	envsData := []string{}
	var err error

	if len(container.Env) > 0 {
		envs, envsData, err = createEnvFile(ctx, config, podData, container)
		if err != nil {
			log.G(ctx).Error(err)
			return nil
		}
	}

	duration := time.Now().UnixMicro() - start
	span.AddEvent("Prepared ENVs for container "+container.Name, trace.WithAttributes(
		attribute.String("prepareenvs.container.name", container.Name),
		attribute.Int64("prepareenvs.duration", duration),
		attribute.StringSlice("prepareenvs.container.envs", envs),
		attribute.StringSlice("prepareenvs.container.envsData", envsData)))

	return envs
}

func getRetrievedContainer(podData *commonIL.RetrievedPodData, containerName string) (*commonIL.RetrievedContainer, error) {
	for _, container := range podData.Containers {
		if container.Name == containerName {
			return &container, nil
		}
	}
	return nil, fmt.Errorf("could not find retrieved container for %s in pod %s", containerName, podData.Pod.Name)
}

func getRetrievedConfigMap(retrievedContainer *commonIL.RetrievedContainer, configMapName string, containerName string, podName string) (*v1.ConfigMap, error) {
	for _, configMap := range retrievedContainer.ConfigMaps {
		if configMap.Name == configMapName {
			return &configMap, nil
		}
	}
	return nil, fmt.Errorf("could not find configMap %s in container %s in pod %s", configMapName, containerName, podName)
}

func getRetrievedProjectedVolumeMap(retrievedContainer *commonIL.RetrievedContainer, projectedVolumeMapName string, _ string, _ string) (*v1.ConfigMap, error) {
	for _, retrievedProjectedVolumeMap := range retrievedContainer.ProjectedVolumeMaps {
		if retrievedProjectedVolumeMap.Name == projectedVolumeMapName {
			return &retrievedProjectedVolumeMap, nil
		}
	}
	// This should not happen, either this is an error or the flag DisableProjectedVolumes is true in VK. Building context for log.
	return nil, nil
}

func getRetrievedSecret(retrievedContainer *commonIL.RetrievedContainer, secretName string, containerName string, podName string) (*v1.Secret, error) {
	for _, retrievedSecret := range retrievedContainer.Secrets {
		if retrievedSecret.Name == secretName {
			return &retrievedSecret, nil
		}
	}
	return nil, fmt.Errorf("could not find secret %s in container %s in pod %s", secretName, containerName, podName)
}

func getPodVolume(pod *v1.Pod, volumeName string) (*v1.Volume, error) {
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == volumeName {
			return &vol, nil
		}
	}
	return nil, fmt.Errorf("could not find volume %s in pod %s", volumeName, pod.Name)
}

func prepareMountsSimpleVolume(
	ctx context.Context,
	config Config,
	container *v1.Container,
	workingPath string,
	volumeObject interface{},
	volumeMount v1.VolumeMount,
	volume v1.Volume,
	mountedDataSB *strings.Builder,
) error {
	volumesHostToContainerPaths, _, err := mountData(ctx, config, container, volumeObject, volumeMount, volume, workingPath)
	if err != nil {
		log.G(ctx).Error(err)
		return err
	}

	log.G(ctx).Debug("volumesHostToContainerPaths: ", volumesHostToContainerPaths)

	for _, volumesHostToContainerPath := range volumesHostToContainerPaths {
		mountedDataSB.WriteString(" --bind ")
		mountedDataSB.WriteString(volumesHostToContainerPath)
	}
	return nil
}

// prepareMounts iterates along the struct provided in the data parameter and checks for ConfigMaps, Secrets and EmptyDirs to be mounted.
// For each element found, the mountData function is called.
// In this context, the general case is given by host and container not sharing the file system, so data are stored within ENVS with matching names.
// The content of these ENVS will be written to a text file by the generated SLURM script later, so the container will be able to mount these files.
// The command to write files is appended in the global "prefix" variable.
// It returns a string composed as the singularity --bind command to bind mount directories and files and the first encountered error.
func (h *SidecarHandler) prepareMounts(
	ctx context.Context,
	config Config,
	podData *commonIL.RetrievedPodData,
	container *v1.Container,
	workingPath string,
) (string, error) {
	span := trace.SpanFromContext(ctx)
	start := time.Now().UnixMicro()
	log.G(ctx).Info(span)
	span.AddEvent("Preparing Mounts for container " + container.Name)

	log.G(ctx).Info("-- Preparing mountpoints for ", container.Name)
	var mountedDataSB strings.Builder

	err := os.MkdirAll(workingPath, os.ModePerm)
	if err != nil {
		log.G(ctx).Error(err)
		return "", err
	}
	log.G(ctx).Info("-- Created directory ", workingPath)
	for _, volumeMount := range container.VolumeMounts {
		volumePtr, err := getPodVolume(&podData.Pod, volumeMount.Name)
		volume := *volumePtr
		if err != nil {
			return "", err
		}

		err = h.processVolumeByType(ctx, config, container, workingPath, volume, volumeMount, podData, &mountedDataSB)
		if err != nil {
			return "", err
		}
	}

	mountedData := mountedDataSB.String()
	if last := len(mountedData) - 1; last >= 0 && mountedData[last] == ',' {
		mountedData = mountedData[:last]
	}
	if len(mountedData) == 0 {
		return "", nil
	}
	log.G(ctx).Debug(mountedData)

	duration := time.Now().UnixMicro() - start
	span.AddEvent("Prepared mounts for container "+container.Name, trace.WithAttributes(
		attribute.String("peparemounts.container.name", container.Name),
		attribute.Int64("preparemounts.duration", duration),
		attribute.String("preparemounts.container.mounts", mountedData)))

	return mountedData, nil
}

// produceSLURMScript generates a SLURM script according to data collected.
// It must be called after ENVS and mounts are already set up since
// it relies on "prefix" variable being populated with needed data and ENVS passed in the commands parameter.
// It returns the path to the generated script and the first encountered error.
func produceSLURMScript(
	ctx context.Context,
	config Config,
	podUID string,
	path string,
	metadata metav1.ObjectMeta,
	commands []SingularityCommand,
	resourceLimits ResourceLimits,
	isDefaultCPU bool,
	isDefaultRAM bool,
) (string, error) {
	start := time.Now().UnixMicro()
	span := trace.SpanFromContext(ctx)
	span.AddEvent("Producing SLURM script")

	// Create script files
	fJob, fSh, err := createScriptFiles(ctx, path)
	if err != nil {
		return "", err
	}
	defer fJob.Close()
	defer fSh.Close()

	// Process SLURM flags
	_, sbatchFlagsAsString, err := processSbatchFlags(ctx, metadata, resourceLimits, isDefaultCPU, isDefaultRAM)
	if err != nil {
		return "", err
	}

	// Handle MPI flags
	if err := processMPIFlags(metadata, commands); err != nil {
		return "", err
	}

	// Setup Tsocks configuration
	tsocksPrefix, postfix := setupTsocksConfiguration(ctx, config, podUID)

	// Add annotation-based environment variables
	prefix := tsocksPrefix
	addAnnotationBasedEnvVars(metadata, config, &prefix)

	// Generate sbatch script
	sbatchScript := generateSbatchScript(config, podUID, path, sbatchFlagsAsString, prefix, fSh)

	// Write job.slurm file
	if err := writeJobSlurmFile(ctx, fJob, sbatchScript); err != nil {
		return "", err
	}

	// Write job.sh file
	if err := writeJobScriptContent(ctx, config, path, commands, fSh, postfix); err != nil {
		return "", err
	}

	// Add telemetry
	duration := time.Now().UnixMicro() - start
	span.AddEvent("Produced SLURM script", trace.WithAttributes(
		attribute.String("produceslurmscript.path", fJob.Name()),
		attribute.Int64("preparemounts.duration", duration),
	))

	return fJob.Name(), nil
}

// SLURMBatchSubmit submits the job provided in the path argument to the SLURM queue.
// At this point, it's up to the SLURM scheduler to manage the job.
// Returns the output of the sbatch command and the first encoundered error.
func BatchSubmit(ctx context.Context, config Config, path string) (string, error) {
	log.G(ctx).Info("- Submitting Slurm job")
	shell := exec2.ExecTask{
		Command: "sh",
		Args:    []string{"-c", "\"" + config.Sbatchpath + " " + path + "\""},
		Shell:   true,
	}

	execReturn, err := shell.Execute()
	if err != nil {
		log.G(ctx).Error("Unable to create file " + path)
		return "", err
	}
	execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

	if execReturn.Stderr != "" {
		log.G(ctx).Error("Could not run sbatch: " + execReturn.Stderr)
		return "", errors.New(execReturn.Stderr)
	}
	log.G(ctx).Debug("Job submitted")
	return execReturn.Stdout, nil
}

// handleJidAndPodUid creates a JID file to store the Job ID of the submitted job.
// The output parameter must be the output of SLURMBatchSubmit function and the path
// is the path where to store the JID file.
// It also adds the JID to the JIDs main structure.
// Finally, it stores the namespace and podUID info in the same location, to restore
// status at startup.
// Return the first encountered error.
func handleJidAndPodUID(ctx context.Context, pod v1.Pod, jIDs *map[string]*JidStruct, output string, path string) (string, error) {
	r := regexp.MustCompile(`Submitted batch job (?P<jid>\d+)`)
	jid := r.FindStringSubmatch(output)
	fJID, err := os.Create(path + "/JobID.jid")
	if err != nil {
		log.G(ctx).Error("Can't create jid_file")
		return "", err
	}
	defer fJID.Close()

	fNS, err := os.Create(path + "/PodNamespace.ns")
	if err != nil {
		log.G(ctx).Error("Can't create namespace_file")
		return "", err
	}
	defer fNS.Close()

	fUID, err := os.Create(path + "/PodUID.uid")
	if err != nil {
		log.G(ctx).Error("Can't create PodUID_file")
		return "", err
	}
	defer fUID.Close()

	_, err = fJID.WriteString(jid[1])
	if err != nil {
		log.G(ctx).Error(err)
		return "", err
	}

	(*jIDs)[string(pod.UID)] = &JidStruct{PodUID: string(pod.UID), PodNamespace: pod.Namespace, JID: jid[1]}
	log.G(ctx).Info("Job ID is: " + (*jIDs)[string(pod.UID)].JID)

	_, err = fNS.WriteString(pod.Namespace)
	if err != nil {
		log.G(ctx).Error(err)
		return "", err
	}

	_, err = fUID.WriteString(string(pod.UID))
	if err != nil {
		log.G(ctx).Error(err)
		return "", err
	}

	return (*jIDs)[string(pod.UID)].JID, nil
}

// removeJID delete a JID from the structure
func removeJID(podUID string, jIDs *map[string]*JidStruct) {
	delete(*jIDs, podUID)
}

// deleteContainer checks if a Job has not yet been deleted and, in case, calls the scancel command to abort the job execution.
// It then removes the JID from the main JIDs structure and all the related files on the disk.
// Returns the first encountered error.
func deleteContainer(ctx context.Context, config Config, podUID string, jIDs *map[string]*JidStruct, path string) error {
	log.G(ctx).Info("- Deleting Job for pod " + podUID)
	span := trace.SpanFromContext(ctx)
	if checkIfJidExists(ctx, jIDs, podUID) {
		//nolint:gosec // G204: config.Scancelpath is from trusted configuration
		_, err := exec.Command(config.Scancelpath, (*jIDs)[podUID].JID).Output()
		if err != nil {
			log.G(ctx).Error(err)
			return err
		}
		log.G(ctx).Info("- Deleted Job ", (*jIDs)[podUID].JID)
	}
	jid := (*jIDs)[podUID].JID
	removeJID(podUID, jIDs)

	errFirstAttempt := os.RemoveAll(path)
	span.SetAttributes(
		attribute.String("delete.pod.uid", podUID),
		attribute.String("delete.jid", jid),
	)

	if errFirstAttempt != nil {
		log.G(ctx).Debug("Attempt 1 of deletion failed, not really an error! Probably log file still opened, waiting for close... Error: ", errFirstAttempt)
		// We expect first rm of directory to possibly fail, in case for eg logs are in follow mode, so opened. The removeJID will end the follow loop,
		// maximum after the loop period of 4s. So we ignore the error and attempt a second time after being sure the loop has ended.
		time.Sleep(5 * time.Second)

		errSecondAttempt := os.RemoveAll(path)
		if errSecondAttempt != nil {
			log.G(ctx).Error("Attempt 2 of deletion failed: ", errSecondAttempt)
			span.AddEvent("Failed to delete SLURM Job " + jid + " for Pod " + podUID)
			return errSecondAttempt
		}
		log.G(ctx).Info("Attempt 2 of deletion succeeded!")
	}
	span.AddEvent("SLURM Job " + jid + " for Pod " + podUID + " successfully deleted")

	// We ignore the deletion error because it is already logged, and because InterLink can still be opening files (eg logs in follow mode).
	// Once InterLink will not use files, all files will be deleted then.
	return nil
}

// For simple volume type like configMap, secret, projectedVolumeMap.
func mountDataSimpleVolume(
	ctx context.Context,
	container *v1.Container,
	path string,
	span trace.Span,
	volumeMount v1.VolumeMount,
	mountDataFiles map[string][]byte,
	start int64,
	volumeType string,
	fileMode os.FileMode,
) ([]string, []string, error) {
	span.AddEvent("Preparing " + volumeType + " mount")

	// Slice of elements of "[host path]:[container volume mount path]"
	var volumesHostToContainerPaths []string
	var envVarNames []string

	err := os.RemoveAll(path + "/" + volumeType + "/" + volumeMount.Name)
	if err != nil {
		log.G(ctx).Error("Unable to delete root folder")
		return []string{}, nil, err
	}

	log.G(ctx).Info("--- Mounting ", volumeType, ": "+volumeMount.Name)
	podVolumeDir := filepath.Join(path, volumeType, volumeMount.Name)

	for key := range mountDataFiles {
		fullPath := filepath.Join(podVolumeDir, key)
		hexString := stringToHex(fullPath)
		mode := ""
		if volumeMount.ReadOnly {
			mode = ":ro"
		} else {
			mode = ":rw"
		}
		// fullPath += (":" + volumeMount.MountPath + "/" + key + mode + " ")
		// volumesHostToContainerPaths = append(volumesHostToContainerPaths, fullPath)

		var containerPath string
		if volumeMount.SubPath != "" {
			containerPath = volumeMount.MountPath
		} else {
			containerPath = filepath.Join(volumeMount.MountPath, key)
		}

		bind := fullPath + ":" + containerPath + mode + " "
		volumesHostToContainerPaths = append(volumesHostToContainerPaths, bind)

		if os.Getenv("SHARED_FS") != sharedFSTrue {
			currentEnvVarName := container.Name + "_" + volumeType + "_" + hexString
			log.G(ctx).Debug("---- Setting env " + currentEnvVarName + " to mount the file later")
			err = os.Setenv(currentEnvVarName, string(mountDataFiles[key]))
			if err != nil {
				log.G(ctx).Error("--- Shared FS disabled, unable to set ENV for ", volumeType, "key: ", key, " env name: ", currentEnvVarName)
				return []string{}, nil, err
			}
			envVarNames = append(envVarNames, currentEnvVarName)
		}
	}

	if os.Getenv("SHARED_FS") == sharedFSTrue {
		log.G(ctx).Info("--- Shared FS enabled, files will be directly created before the job submission")
		err := os.MkdirAll(podVolumeDir, os.FileMode(0755)|os.ModeDir)
		if err != nil {
			return []string{}, nil, fmt.Errorf("could not create whole directory of %s root cause %w", podVolumeDir, err)
		}
		log.G(ctx).Debug("--- Created folder ", podVolumeDir)
		/*
			cmd := []string{"-p " + podVolumeDir}
			shell := exec2.ExecTask{
				Command: "mkdir",
				Args:    cmd,
				Shell:   true,
			}

			execReturn, err := shell.Execute()
			if strings.Compare(execReturn.Stdout, "") != 0 {
				log.G(ctx).Error(err)
				return []string{}, nil, err
			}
			if execReturn.Stderr != "" {
				log.G(ctx).Error(execReturn.Stderr)
				return []string{}, nil, err
			} else {
				log.G(ctx).Debug("--- Created folder " + podVolumeDir)
			}
		*/

		log.G(ctx).Debug("--- Writing ", volumeType, " files")
		for k, v := range mountDataFiles {
			// TODO: Ensure that these files are deleted in failure cases
			fullPath := filepath.Join(podVolumeDir, k)

			// mode := os.FileMode(0644)
			err := os.WriteFile(fullPath, v, fileMode)
			if err != nil {
				log.G(ctx).Errorf("Could not write %s file %s", volumeType, fullPath)
				err = os.RemoveAll(fullPath)
				if err != nil {
					log.G(ctx).Error("Unable to remove file ", fullPath)
					return []string{}, nil, err
				}
				return []string{}, nil, err
			}
			log.G(ctx).Debugf("--- Written %s file %s", volumeType, fullPath)
		}
	}
	duration := time.Now().UnixMicro() - start
	span.AddEvent("Prepared "+volumeType+" mounts", trace.WithAttributes(
		attribute.String("mountdata.container.name", container.Name),
		attribute.Int64("mountdata.duration", duration),
		attribute.StringSlice("mountdata.container."+volumeType, volumesHostToContainerPaths)))
	return volumesHostToContainerPaths, envVarNames, nil
}

/*
mountData is called by prepareMounts and creates files and directory according to their definition in the pod structure.
The data parameter is an interface and it can be of type v1.ConfigMap, v1.Secret and string (for the empty dir).

Returns:
volumesHostToContainerPaths:

	Each path is one file (not a directory). Eg for configMap that contains one file "file1" et one "file2".
	volumesHostToContainerPaths := ["/path/to/file1:/path/container/file1:rw", "/path/to/file2:/path/container/file2:rw",]

envVarNames:

	For SHARED_FS = false mode. Each one is the environment variable name matching each item of volumesHostToContainerPaths (in the same order),
	to be used to create the files inside the container.

error:

	The first encountered error, or nil
*/
func mountData(ctx context.Context, config Config, container *v1.Container, retrievedDataObject interface{}, volumeMount v1.VolumeMount, volume v1.Volume, path string) ([]string, []string, error) {
	span := trace.SpanFromContext(ctx)
	start := time.Now().UnixMicro()
	if config.ExportPodData {
		// for _, mountSpec := range container.VolumeMounts {
		switch retrievedDataObjectCasted := retrievedDataObject.(type) {
		case v1.ConfigMap:
			var volumeType string
			var defaultMode *int32
			if volume.ConfigMap != nil {
				volumeType = "configMaps"
				defaultMode = volume.ConfigMap.DefaultMode
			} else if volume.Projected != nil {
				volumeType = "projectedVolumeMaps"
				defaultMode = volume.Projected.DefaultMode
			}

			log.G(ctx).Debugf("in mountData() volume found: %s type: %s", volumeMount.Name, volumeType)

			// Convert map of string to map of []byte
			mountDataConfigMapsAsBytes := make(map[string][]byte)
			for key := range retrievedDataObjectCasted.Data {
				mountDataConfigMapsAsBytes[key] = []byte(retrievedDataObjectCasted.Data[key])
			}
			fileMode := os.FileMode(*defaultMode)
			return mountDataSimpleVolume(ctx, container, path, span, volumeMount, mountDataConfigMapsAsBytes, start, volumeType, fileMode)

		case v1.Secret:
			volumeType := "secrets"
			log.G(ctx).Debugf("in mountData() volume found: %s type: %s", volumeMount.Name, volumeType)

			fileMode := os.FileMode(*volume.Secret.DefaultMode)
			return mountDataSimpleVolume(ctx, container, path, span, volumeMount, retrievedDataObjectCasted.Data, start, volumeType, fileMode)

		case string:
			span.AddEvent("Preparing EmptyDirs mount")
			var edPaths []string
			if volume.EmptyDir != nil {
				log.G(ctx).Debugf("in mountData() volume found: %s type: emptyDir", volumeMount.Name)

				var edPath string
				edPath = filepath.Join(path, "emptyDirs", volume.Name)
				log.G(ctx).Info("-- Creating EmptyDir in ", edPath)
				err := os.MkdirAll(edPath, os.FileMode(0755)|os.ModeDir)
				if err != nil {
					return []string{}, nil, fmt.Errorf("could not create whole directory of %s root cause %w", edPath, err)
				}
				log.G(ctx).Debug("-- Created EmptyDir in ", edPath)
				/*
					cmd := []string{"-p " + edPath}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					_, err := shell.Execute()
					if err != nil {
						log.G(ctx).Error(err)
						return []string{}, nil, err
					} else {
						log.G(ctx).Debug("-- Created EmptyDir in ", edPath)
					}
				*/

				mode := ""
				if volumeMount.ReadOnly {
					mode = ":ro"
				} else {
					mode = ":rw"
				}
				edPath += (":" + volumeMount.MountPath + mode + " ")
				edPaths = append(edPaths, " --bind "+edPath+" ")
			}
			duration := time.Now().UnixMicro() - start
			span.AddEvent("Prepared emptydir mounts", trace.WithAttributes(
				attribute.String("mountdata.container.name", container.Name),
				attribute.Int64("mountdata.duration", duration),
				attribute.StringSlice("mountdata.container.emptydirs", edPaths)))
			return edPaths, nil, nil

		default:
			log.G(ctx).Warningf("in mountData() volume %s with unknown retrievedDataObject", volumeMount.Name)
		}
	}
	return nil, nil, nil
}

// checkIfJidExists checks if a JID is in the main JIDs struct
func checkIfJidExists(ctx context.Context, jIDs *map[string]*JidStruct, uid string) bool {
	span := trace.SpanFromContext(ctx)
	_, ok := (*jIDs)[uid]

	if ok {
		return true
	}
	span.AddEvent("Span for PodUID " + uid + " doesn't exist")
	return false
}

// getExitCode returns the exit code read from the .status file of a specific container and returns it as an int32 number
func getExitCode(ctx context.Context, path string, ctName string, exitCodeMatch string, sessionContextMessage string) (int32, error) {
	statusFilePath := path + "/run-" + ctName + ".status"
	exitCode, err := os.ReadFile(statusFilePath)
	if err != nil {
		statusFilePath = path + "/init-" + ctName + ".status"
		exitCode, err = os.ReadFile(statusFilePath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Case job terminated before the container script has the time to write status file (eg: canceled jobs).
				log.G(ctx).Warning(sessionContextMessage, "file ", statusFilePath, " not found despite the job being in terminal state. Workaround: using Slurm job exit code:", exitCodeMatch)

				exitCodeInt, errAtoi := strconv.Atoi(exitCodeMatch)
				if errAtoi != nil {
					errWithContext := fmt.Errorf(sessionContextMessage+"error during Atoi() of getExitCode() of file %s exitCodeMatch: %s error: %s %w", statusFilePath, exitCodeMatch, fmt.Sprintf("%#v", errAtoi), errAtoi)
					log.G(ctx).Error(errWithContext)
					return 11, errWithContext
				}
				errWriteFile := os.WriteFile(statusFilePath, []byte(exitCodeMatch), 0600)
				if errWriteFile != nil {
					errWithContext := fmt.Errorf(sessionContextMessage+"error during WriteFile() of getExitCode() of file %s error: %s %w", statusFilePath, fmt.Sprintf("%#v", errWriteFile), errWriteFile)
					log.G(ctx).Error(errWithContext)
					return 12, errWithContext
				}
				return int32(exitCodeInt), nil
			}
			errWithContext := fmt.Errorf(sessionContextMessage+"error during ReadFile() of getExitCode() of file %s error: %s %w", statusFilePath, fmt.Sprintf("%#v", err), err)
			return 21, errWithContext
		}
	}
	exitCodeInt, err := strconv.Atoi(strings.ReplaceAll(string(exitCode), "\n", ""))
	if err != nil {
		log.G(ctx).Error(err)
		return 0, err
	}
	return int32(exitCodeInt), nil
}
