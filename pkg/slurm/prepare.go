package slurm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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
	Config SlurmConfig
	JIDs   *map[string]*JidStruct
	Ctx    context.Context
}

var prefix string
var timer time.Time
var cachedStatus []commonIL.PodStatus

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
func parsingTimeFromString(Ctx context.Context, stringTime string, timestampFormat string) (time.Time, error) {
	parsedTime := time.Time{}
	parts := strings.Fields(stringTime)
	if len(parts) != 4 {
		err := errors.New("invalid timestamp format")
		log.G(Ctx).Error(err)
		return time.Time{}, err
	}

	parsedTime, err := time.Parse(timestampFormat, stringTime)
	if err != nil {
		log.G(Ctx).Error(err)
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
			} else {
				podUID, err = os.ReadFile(path + entry.Name() + "/" + "PodUID.uid")
				if err != nil {
					log.G(h.Ctx).Debug(err)
					continue
				} else {
					podNamespace, err = os.ReadFile(path + entry.Name() + "/" + "PodNamespace.ns")
					if err != nil {
						log.G(h.Ctx).Debug(err)
						continue
					}
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

func createEnvFile(Ctx context.Context, config SlurmConfig, podData commonIL.RetrievedPodData, container v1.Container) ([]string, []string, error) {
		envs := []string{}
		// For debugging purpose only
		envs_data := []string{}
	
		envfilePath := (config.DataRootFolder + podData.Pod.Namespace + "-" + string(podData.Pod.UID) + "/" + "envfile.properties")
		log.G(Ctx).Info("-- Appending envs using envfile " + envfilePath)
		envs = append(envs, "--env-file")
		envs = append(envs, envfilePath)
		
		envfile, err := os.Create(envfilePath)
		if err != nil {
			log.G(Ctx).Error(err)
			return nil, nil, err
		}
		defer envfile.Close()
		
		for _, envVar := range container.Env {
			// The environment variable values can contains all sort of simple/double quote and space and any arbitrary values.
			// singularity reads the env-file and parse it like a bash string, so shellescape will escape any quote properly.
			tmpValue :=  shellescape.Quote(envVar.Value)
			tmp := (envVar.Name + "=" + tmpValue)
			
			envs_data = append(envs_data, tmp)
			
			_, err := envfile.WriteString(tmp + "\n")
			if err != nil {
				log.G(Ctx).Error(err)
				return nil, nil, err
			} else {
				log.G(Ctx).Debug("---- Written envfile file " + envfilePath + " key " + envVar.Name + " value " + tmpValue)
			}
		}
		
		// All env variables are written, we flush it now. 
		err = envfile.Sync()
		if err != nil {
			log.G(Ctx).Error(err)
			return nil, nil, err
		}
		
		// Calling Close() in case of error. If not error, the defer will close it again but it should be idempotent.
		envfile.Close()
		
		return envs, envs_data, nil
}

// prepareEnvs reads all Environment variables from a container and append them to a envfile.properties. The values are bash-escaped.
// It returns the slice containing, if there are Environment variables, the arguments for envfile and its path, or else an empty array.
func prepareEnvs(Ctx context.Context, config SlurmConfig, podData commonIL.RetrievedPodData, container v1.Container) []string {
	start := time.Now().UnixMicro()
	span := trace.SpanFromContext(Ctx)
	span.AddEvent("Preparing ENVs for container " + container.Name)
	var envs []string = []string{}
	// For debugging purpose only
	envs_data := []string{}
	var err error

	if len(container.Env) > 0 {
		envs, envs_data, err = createEnvFile(Ctx, config, podData, container)
		if err != nil {
			log.G(Ctx).Error(err)
			return nil
		}
	}

	duration := time.Now().UnixMicro() - start
	span.AddEvent("Prepared ENVs for container "+container.Name, trace.WithAttributes(
		attribute.String("prepareenvs.container.name", container.Name),
		attribute.Int64("prepareenvs.duration", duration),
		attribute.StringSlice("prepareenvs.container.envs", envs),
		attribute.StringSlice("prepareenvs.container.envs_data", envs_data)))

	return envs
}

// prepareMounts iterates along the struct provided in the data parameter and checks for ConfigMaps, Secrets and EmptyDirs to be mounted.
// For each element found, the mountData function is called.
// In this context, the general case is given by host and container not sharing the file system, so data are stored within ENVS with matching names.
// The content of these ENVS will be written to a text file by the generated SLURM script later, so the container will be able to mount these files.
// The command to write files is appended in the global "prefix" variable.
// It returns a string composed as the singularity --bind command to bind mount directories and files and the first encountered error.
func prepareMounts(
	Ctx context.Context,
	config SlurmConfig,
	podData commonIL.RetrievedPodData,
	container v1.Container,
	workingPath string,
) (string, error) {
	span := trace.SpanFromContext(Ctx)
	start := time.Now().UnixMicro()
	log.G(Ctx).Info(span)
	span.AddEvent("Preparing Mounts for container " + container.Name)

	log.G(Ctx).Info("-- Preparing mountpoints for " + container.Name)
	mountedData := ""

	err := os.MkdirAll(workingPath, os.ModePerm)
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	} else {
		log.G(Ctx).Info("-- Created directory " + workingPath)
	}

	for _, cont := range podData.Containers {
		for _, cfgMap := range cont.ConfigMaps {
			if container.Name == cont.Name {
				configMapPath, env, err := mountData(Ctx, config, podData.Pod, container, cfgMap, workingPath)
				if err != nil {
					log.G(Ctx).Error(err)
					return "", err
				}

				log.G(Ctx).Debug(configMapPath)

				for _, cfgMapPath := range configMapPath {
					if os.Getenv("SHARED_FS") != "true" {
						dirs := strings.Split(cfgMapPath, ":")
						splitDirs := strings.Split(dirs[0], "/")
						dir := filepath.Join(splitDirs[:len(splitDirs)-1]...)
						prefix += "\nmkdir -p " + dir + " && touch " + dirs[0] + " && echo $" + env + " > " + dirs[0]
					}
					mountedData += " --bind " + cfgMapPath
				}
			}
		}

		for _, secret := range cont.Secrets {
			if container.Name == cont.Name {
				secretPath, env, err := mountData(Ctx, config, podData.Pod, container, secret, workingPath)
				if err != nil {
					log.G(Ctx).Error(err)
					return "", err
				}

				log.G(Ctx).Debug(secretPath)

				for _, scrtPath := range secretPath {
					if os.Getenv("SHARED_FS") != "true" {
						dirs := strings.Split(scrtPath, ":")
						splitDirs := strings.Split(dirs[0], "/")
						dir := filepath.Join(splitDirs[:len(splitDirs)-1]...)
						splittedEnv := strings.Split(env, "_")
						log.G(Ctx).Info(splittedEnv[len(splittedEnv)-1])
						prefix += "\nmkdir -p " + dir + " && touch " + dirs[0] + " && echo $" + env + " > " + dirs[0]
					}
					mountedData += " --bind " + scrtPath
				}
			}
		}

		if container.Name == cont.Name {
			edPath, _, err := mountData(Ctx, config, podData.Pod, container, "emptyDir", workingPath)
			if err != nil {
				log.G(Ctx).Error(err)
				return "", err
			}

			log.G(Ctx).Debug(edPath)

			for _, mntData := range edPath {
				mountedData += mntData
			}
		}

	}

	if last := len(mountedData) - 1; last >= 0 && mountedData[last] == ',' {
		mountedData = mountedData[:last]
	}
	if len(mountedData) == 0 {
		return "", nil
	}
	log.G(Ctx).Debug(mountedData)

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
	Ctx context.Context,
	config SlurmConfig,
	podUID string,
	path string,
	metadata metav1.ObjectMeta,
	commands []SingularityCommand,
	resourceLimits ResourceLimits,
) (string, error) {
	start := time.Now().UnixMicro()
	span := trace.SpanFromContext(Ctx)
	span.AddEvent("Producing SLURM script")

	log.G(Ctx).Info("-- Creating file for the Slurm script")
	prefix = ""
	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	} else {
		log.G(Ctx).Info("-- Created directory " + path)
	}
	postfix := ""

	f, err := os.Create(path + "/job.sh")
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	}
	err = os.Chmod(path+"/job.sh", 0774)
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	}
	defer f.Close()

	if err != nil {
		log.G(Ctx).Error("Unable to create file " + path + "/job.sh")
		return "", err
	} else {
		log.G(Ctx).Debug("--- Created file " + path + "/job.sh")
	}

	var sbatchFlagsFromArgo []string
	var sbatchFlagsAsString = ""
	if slurmFlags, ok := metadata.Annotations["slurm-job.vk.io/flags"]; ok {
		sbatchFlagsFromArgo = strings.Split(slurmFlags, " ")
	}
	if mpiFlags, ok := metadata.Annotations["slurm-job.vk.io/mpi-flags"]; ok {
		if mpiFlags != "true" {
			mpi := append([]string{"mpiexec", "-np", "$SLURM_NTASKS"}, strings.Split(mpiFlags, " ")...)
			for _, singularityCommand := range commands {
				singularityCommand.singularityCommand = append(mpi, singularityCommand.singularityCommand...)
			}
		}
	}

	sbatchFlagsFromArgo = append(sbatchFlagsFromArgo, "--mem="+strconv.FormatInt(resourceLimits.Memory/1024/1024, 10))
	sbatchFlagsFromArgo = append(sbatchFlagsFromArgo, "--cpus-per-task="+strconv.FormatInt(resourceLimits.CPU, 10))

	for _, slurmFlag := range sbatchFlagsFromArgo {
		sbatchFlagsAsString += "\n#SBATCH " + slurmFlag
	}

	if config.Tsocks {
		log.G(Ctx).Debug("--- Adding SSH connection and setting ENVs to use TSOCKS")
		postfix += "\n\nkill -15 $SSH_PID &> log2.txt"

		prefix += "\n\nmin_port=10000"
		prefix += "\nmax_port=65000"
		prefix += "\nfor ((port=$min_port; port<=$max_port; port++))"
		prefix += "\ndo"
		prefix += "\n  temp=$(ss -tulpn | grep :$port)"
		prefix += "\n  if [ -z \"$temp\" ]"
		prefix += "\n  then"
		prefix += "\n    break"
		prefix += "\n  fi"
		prefix += "\ndone"

		prefix += "\nssh -4 -N -D $port " + config.Tsockslogin + " &"
		prefix += "\nSSH_PID=$!"
		prefix += "\necho \"local = 10.0.0.0/255.0.0.0 \nserver = 127.0.0.1 \nserver_port = $port\" >> .tmp/" + podUID + "_tsocks.conf"
		prefix += "\nexport TSOCKS_CONF_FILE=.tmp/" + podUID + "_tsocks.conf && export LD_PRELOAD=" + config.Tsockspath
	}

	if config.Commandprefix != "" {
		prefix += "\n" + config.Commandprefix
	}

	if preExecAnnotations, ok := metadata.Annotations["slurm-job.vk.io/pre-exec"]; ok {
		prefix += "\n" + preExecAnnotations
	}

	sbatch_macros := "#!" + config.BashPath +
		"\n#SBATCH --job-name=" + podUID +
		"\n#SBATCH --output=" + path + "/job.out" +
		sbatchFlagsAsString +
		"\n" +
		prefix +
		"\n"

	log.G(Ctx).Debug("--- Writing file")

	var stringToBeWritten strings.Builder

	stringToBeWritten.WriteString(sbatch_macros)

	for _, singularityCommand := range commands {

		stringToBeWritten.WriteString("\n")
		stringToBeWritten.WriteString(strings.Join(singularityCommand.singularityCommand[:], " "))

		if singularityCommand.containerCommand != nil {
			// Case the pod specified a container entrypoint array to override.
			for _, commandEntry := range singularityCommand.containerCommand {
				stringToBeWritten.WriteString(" ")
				// We convert from GO array to shell command, so escaping is important to avoid space, quote issues and injection vulnerabilities.
				stringToBeWritten.WriteString(shellescape.Quote(commandEntry))
			}
		}
		if singularityCommand.containerArgs != nil {
			// Case the pod specified a container command array to override.
			for _, argsEntry := range singularityCommand.containerArgs {
				stringToBeWritten.WriteString(" ")
				// We convert from GO array to shell command, so escaping is important to avoid space, quote issues and injection vulnerabilities.
				stringToBeWritten.WriteString(shellescape.Quote(argsEntry))
			}
		}

		stringToBeWritten.WriteString(" &> ")
		stringToBeWritten.WriteString(path)
		stringToBeWritten.WriteString("/")
		stringToBeWritten.WriteString(singularityCommand.containerName)
		stringToBeWritten.WriteString(".out; ")
		stringToBeWritten.WriteString("echo $? > " + path + "/" + singularityCommand.containerName + ".status")
		
		if ! singularityCommand.isInitContainer {
			// Not init containers are run in parallel.
			stringToBeWritten.WriteString("; sleep 30 &")
		}
	}

	stringToBeWritten.WriteString("\n")
	stringToBeWritten.WriteString(postfix)

	_, err = f.WriteString(stringToBeWritten.String())

	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	} else {
		log.G(Ctx).Debug("---- Written file")
	}

	duration := time.Now().UnixMicro() - start
	span.AddEvent("Produced SLURM script", trace.WithAttributes(
		attribute.String("produceslurmscript.path", f.Name()),
		attribute.Int64("preparemounts.duration", duration),
	))

	return f.Name(), nil
}

// SLURMBatchSubmit submits the job provided in the path argument to the SLURM queue.
// At this point, it's up to the SLURM scheduler to manage the job.
// Returns the output of the sbatch command and the first encoundered error.
func SLURMBatchSubmit(Ctx context.Context, config SlurmConfig, path string) (string, error) {
	log.G(Ctx).Info("- Submitting Slurm job")
	shell := exec2.ExecTask{
		Command: "sh",
		Args:    []string{"-c", "\"" + config.Sbatchpath + " " + path + "\""},
		Shell:   true,
	}

	execReturn, err := shell.Execute()
	if err != nil {
		log.G(Ctx).Error("Unable to create file " + path)
		return "", err
	}
	execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

	if execReturn.Stderr != "" {
		log.G(Ctx).Error("Could not run sbatch: " + execReturn.Stderr)
		return "", errors.New(execReturn.Stderr)
	} else {
		log.G(Ctx).Debug("Job submitted")
	}
	return string(execReturn.Stdout), nil
}

// handleJidAndPodUid creates a JID file to store the Job ID of the submitted job.
// The output parameter must be the output of SLURMBatchSubmit function and the path
// is the path where to store the JID file.
// It also adds the JID to the JIDs main structure.
// Finally, it stores the namespace and podUID info in the same location, to restore
// status at startup.
// Return the first encountered error.
func handleJidAndPodUid(Ctx context.Context, pod v1.Pod, JIDs *map[string]*JidStruct, output string, path string) (string, error) {
	r := regexp.MustCompile(`Submitted batch job (?P<jid>\d+)`)
	jid := r.FindStringSubmatch(output)
	fJID, err := os.Create(path + "/JobID.jid")
	if err != nil {
		log.G(Ctx).Error("Can't create jid_file")
		return "", err
	}
	defer fJID.Close()

	fNS, err := os.Create(path + "/PodNamespace.ns")
	if err != nil {
		log.G(Ctx).Error("Can't create namespace_file")
		return "", err
	}
	defer fNS.Close()

	fUID, err := os.Create(path + "/PodUID.uid")
	if err != nil {
		log.G(Ctx).Error("Can't create PodUID_file")
		return "", err
	}
	defer fUID.Close()

	_, err = fJID.WriteString(jid[1])
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	}

	(*JIDs)[string(pod.UID)] = &JidStruct{PodUID: string(pod.UID), PodNamespace: pod.Namespace, JID: jid[1]}
	log.G(Ctx).Info("Job ID is: " + (*JIDs)[string(pod.UID)].JID)

	_, err = fNS.WriteString(pod.Namespace)
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	}

	_, err = fUID.WriteString(string(pod.UID))
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	}

	return (*JIDs)[string(pod.UID)].JID, nil
}

// removeJID delete a JID from the structure
func removeJID(podUID string, JIDs *map[string]*JidStruct) {
	delete(*JIDs, podUID)
}

// deleteContainer checks if a Job has not yet been deleted and, in case, calls the scancel command to abort the job execution.
// It then removes the JID from the main JIDs structure and all the related files on the disk.
// Returns the first encountered error.
func deleteContainer(Ctx context.Context, config SlurmConfig, podUID string, JIDs *map[string]*JidStruct, path string) error {
	log.G(Ctx).Info("- Deleting Job for pod " + podUID)
	span := trace.SpanFromContext(Ctx)
	if checkIfJidExists(Ctx, JIDs, podUID) {
		_, err := exec.Command(config.Scancelpath, (*JIDs)[podUID].JID).Output()
		if err != nil {
			log.G(Ctx).Error(err)
			return err
		} else {
			log.G(Ctx).Info("- Deleted Job ", (*JIDs)[podUID].JID)
		}
	}
	err := os.RemoveAll(path)
	jid := (*JIDs)[podUID].JID
	removeJID(podUID, JIDs)

	span.SetAttributes(
		attribute.String("delete.pod.uid", podUID),
		attribute.String("delete.jid", jid),
	)

	if err != nil {
		log.G(Ctx).Error(err)
		span.AddEvent("Failed to delete SLURM Job " + (*JIDs)[podUID].JID + " for Pod " + podUID)
	} else {
		span.AddEvent("SLURM Job " + jid + " for Pod " + podUID + " successfully deleted")
	}

	return err
}

// mountData is called by prepareMounts and creates files and directory according to their definition in the pod structure.
// The data parameter is an interface and it can be of type v1.ConfigMap, v1.Secret and string (for the empty dir).
// Returns 2 slices of string, one containing the ConfigMaps/Secrets/EmptyDirs paths and one the list of relatives ENVS to be used
// to create the files inside the container.
// It also returns the first encountered error.
func mountData(Ctx context.Context, config SlurmConfig, pod v1.Pod, container v1.Container, data interface{}, path string) ([]string, string, error) {
	span := trace.SpanFromContext(Ctx)
	start := time.Now().UnixMicro()
	if config.ExportPodData {
		for _, mountSpec := range container.VolumeMounts {
			switch mount := data.(type) {
			case v1.ConfigMap:
				span.AddEvent("Preparing ConfigMap mount")
				for _, vol := range pod.Spec.Volumes {
					if vol.ConfigMap != nil && vol.Name == mountSpec.Name && mount.Name == vol.ConfigMap.Name {
						configMaps := make(map[string]string)
						var configMapNamePath []string
						var env string

						err := os.RemoveAll(path + "/configMaps/" + mount.Name)
						if err != nil {
							log.G(Ctx).Error("Unable to delete root folder")
							return []string{}, "", err
						}

						//if podVolumeSpec != nil && podVolumeSpec.ConfigMap != nil {
						log.G(Ctx).Info("--- Mounting ConfigMap " + mountSpec.Name)
						//mode := os.FileMode(*podVolumeSpec.ConfigMap.DefaultMode)
						mode := os.FileMode(0644)
						podConfigMapDir := filepath.Join(path+"/", "configMaps/", mountSpec.Name)

						for key := range mount.Data {
							configMaps[key] = mount.Data[key]
							fullPath := filepath.Join(podConfigMapDir, key)
							hexString := stringToHex(fullPath)
							mode := ""
							if mountSpec.ReadOnly {
								mode = ":ro"
							} else {
								mode = ":rw"
							}
							fullPath += (":" + mountSpec.MountPath + "/" + key + mode + " ")
							configMapNamePath = append(configMapNamePath, fullPath)

							if os.Getenv("SHARED_FS") != "true" {
								envTemp := string(container.Name) + "_CFG_" + string(hexString)
								log.G(Ctx).Debug("---- Setting env " + env + " to mount the file later")
								err = os.Setenv(env, mount.Data[key])
								if err != nil {
									log.G(Ctx).Error("Unable to set ENV for cfgmap " + key)
									return []string{}, "", err
								}
								env = envTemp
							}
						}

						if os.Getenv("SHARED_FS") == "true" {
							log.G(Ctx).Info("--- Shared FS enabled, files will be directly created before the job submission")
							cmd := []string{"-p " + podConfigMapDir}
							shell := exec2.ExecTask{
								Command: "mkdir",
								Args:    cmd,
								Shell:   true,
							}

							execReturn, err := shell.Execute()

							if err != nil {
								log.G(Ctx).Error(err)
								return []string{}, "", err
							} else if execReturn.Stderr != "" {
								log.G(Ctx).Error(execReturn.Stderr)
								return []string{}, "", errors.New(execReturn.Stderr)
							} else {
								log.G(Ctx).Debug("--- Created folder " + podConfigMapDir)
							}

							log.G(Ctx).Debug("--- Writing ConfigMaps files")
							for k, v := range configMaps {
								// TODO: Ensure that these files are deleted in failure cases
								fullPath := filepath.Join(podConfigMapDir, k)
								err = os.WriteFile(fullPath, []byte(v), mode)
								if err != nil {
									log.G(Ctx).Errorf("Could not write ConfigMap file %s", fullPath)
									os.RemoveAll(fullPath)
									if err != nil {
										log.G(Ctx).Error("Unable to remove file " + fullPath)
										return []string{}, "", err
									}
									return []string{}, "", err
								} else {
									log.G(Ctx).Debug("Written ConfigMap file " + fullPath)
								}
							}
						}
						duration := time.Now().UnixMicro() - start
						span.AddEvent("Prepared ConfigMap mounts", trace.WithAttributes(
							attribute.String("mountdata.container.name", container.Name),
							attribute.Int64("mountdata.duration", duration),
							attribute.StringSlice("mountdata.container.configmaps", configMapNamePath)))
						return configMapNamePath, env, nil
					}
				}
				//}

			case v1.Secret:
				span.AddEvent("Preparing ConfigMap mount")
				for _, vol := range pod.Spec.Volumes {
					if vol.Secret != nil && vol.Name == mountSpec.Name && mount.Name == vol.Secret.SecretName {
						secrets := make(map[string][]byte)
						var secretNamePath []string
						var env string

						err := os.RemoveAll(path + "/secrets/" + mountSpec.Name)

						if err != nil {
							log.G(Ctx).Error("Unable to delete root folder")
							return []string{}, "", err
						}

						//if podVolumeSpec != nil && podVolumeSpec.Secret != nil {
						log.G(Ctx).Info("--- Mounting Secret " + mountSpec.Name)
						mode := os.FileMode(0644)
						podSecretDir := filepath.Join(path+"/", "secrets/", mountSpec.Name)

						if mount.Data != nil {
							for key := range mount.Data {
								secrets[key] = mount.Data[key]
								fullPath := filepath.Join(podSecretDir, key)
								hexString := stringToHex(fullPath)
								mode := ""
								if mountSpec.ReadOnly {
									mode = ":ro"
								} else {
									mode = ":rw"
								}
								fullPath += (":" + mountSpec.MountPath + "/" + key + mode + " ")
								secretNamePath = append(secretNamePath, fullPath)

								if os.Getenv("SHARED_FS") != "true" {
									envTemp := string(container.Name) + "_SECRET_" + hexString
									log.G(Ctx).Debug("---- Setting env " + env + " to mount the file later")
									err = os.Setenv(env, string(mount.Data[key]))
									if err != nil {
										log.G(Ctx).Error("Unable to set ENV for secret " + key)
										return []string{}, "", err
									}
									env = envTemp
								}
							}
						}

						if os.Getenv("SHARED_FS") == "true" {
							log.G(Ctx).Info("--- Shared FS enabled, files will be directly created before the job submission")
							cmd := []string{"-p " + podSecretDir}
							shell := exec2.ExecTask{
								Command: "mkdir",
								Args:    cmd,
								Shell:   true,
							}

							execReturn, err := shell.Execute()
							if strings.Compare(execReturn.Stdout, "") != 0 {
								log.G(Ctx).Error(err)
								return []string{}, "", err
							}
							if execReturn.Stderr != "" {
								log.G(Ctx).Error(execReturn.Stderr)
								return []string{}, "", err
							} else {
								log.G(Ctx).Debug("--- Created folder " + podSecretDir)
							}

							log.G(Ctx).Debug("--- Writing Secret files")
							for k, v := range secrets {
								// TODO: Ensure that these files are deleted in failure cases
								fullPath := filepath.Join(podSecretDir, k)
								os.WriteFile(fullPath, v, mode)
								if err != nil {
									log.G(Ctx).Errorf("Could not write Secret file %s", fullPath)
									err = os.RemoveAll(fullPath)
									if err != nil {
										log.G(Ctx).Error("Unable to remove file " + fullPath)
										return []string{}, "", err
									}
									return []string{}, "", err
								} else {
									log.G(Ctx).Debug("--- Written Secret file " + fullPath)
								}
							}
						}
						duration := time.Now().UnixMicro() - start
						span.AddEvent("Prepared Secrets mounts", trace.WithAttributes(
							attribute.String("mountdata.container.name", container.Name),
							attribute.Int64("mountdata.duration", duration),
							attribute.StringSlice("mountdata.container.secrets", secretNamePath)))
						return secretNamePath, env, nil
					}
				}
				//}

			case string:
				span.AddEvent("Preparing EmptyDirs mount")
				var edPaths []string
				for _, vol := range pod.Spec.Volumes {
					for _, mountSpec := range container.VolumeMounts {
						if vol.EmptyDir != nil && vol.Name == mountSpec.Name {
							var edPath string
							edPath = filepath.Join(path + "/" + "emptyDirs/" + vol.Name)
							log.G(Ctx).Info("-- Creating EmptyDir in " + edPath)
							cmd := []string{"-p " + edPath}
							shell := exec2.ExecTask{
								Command: "mkdir",
								Args:    cmd,
								Shell:   true,
							}

							_, err := shell.Execute()
							if err != nil {
								log.G(Ctx).Error(err)
								return []string{}, "", err
							} else {
								log.G(Ctx).Debug("-- Created EmptyDir in " + edPath)
							}

							mode := ""
							if mountSpec.ReadOnly {
								mode = ":ro"
							} else {
								mode = ":rw"
							}
							edPath += (":" + mountSpec.MountPath + mode + " ")
							edPaths = append(edPaths, " --bind "+edPath+" ")
						}
					}
				}
				duration := time.Now().UnixMicro() - start
				span.AddEvent("Prepared Secrets mounts", trace.WithAttributes(
					attribute.String("mountdata.container.name", container.Name),
					attribute.Int64("mountdata.duration", duration),
					attribute.StringSlice("mountdata.container.emptydirs", edPaths)))
				return edPaths, "", nil
			}
		}
	}
	return []string{}, "", nil
}

// checkIfJidExists checks if a JID is in the main JIDs struct
func checkIfJidExists(ctx context.Context, JIDs *map[string]*JidStruct, uid string) bool {
	span := trace.SpanFromContext(ctx)
	_, ok := (*JIDs)[uid]

	if ok {
		return true
	} else {
		span.AddEvent("Span for PodUID " + uid + " doesn't exist")
		return false
	}
}

// getExitCode returns the exit code read from the .status file of a specific container and returns it as an int32 number
func getExitCode(ctx context.Context, path string, ctName string) (int32, error) {
	exitCode, err := os.ReadFile(path + "/" + ctName + ".status")
	if err != nil {
		log.G(ctx).Error(err)
		return 0, err
	}
	exitCodeInt, err := strconv.Atoi(strings.Replace(string(exitCode), "\n", "", -1))
	if err != nil {
		log.G(ctx).Error(err)
		return 0, err
	}
	return int32(exitCodeInt), nil
}
