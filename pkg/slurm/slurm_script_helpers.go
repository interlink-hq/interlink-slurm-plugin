package slurm

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/containerd/containerd/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// processSbatchFlags processes SLURM batch flags from annotations and resource limits
func processSbatchFlags(
	ctx context.Context,
	metadata metav1.ObjectMeta,
	resourceLimits ResourceLimits,
	isDefaultCPU, isDefaultRAM bool,
) ([]string, string, error) {
	cpuLimitSetFromFlags := false
	memoryLimitSetFromFlags := false

	var sbatchFlagsFromArgo []string
	if slurmFlags, ok := metadata.Annotations["slurm-job.vk.io/flags"]; ok {
		reCPU := regexp.MustCompile(`--cpus-per-task(?:[ =]\S+)?`)
		reRAM := regexp.MustCompile(`--mem(?:[ =]\S+)?`)

		// if isDefaultCPU is false, it means that the CPU limit is set in the pod spec, so we ignore the --cpus-per-task flag from annotations.
		if !isDefaultCPU {
			if reCPU.MatchString(slurmFlags) {
				log.G(ctx).Info("Ignoring --cpus-per-task flag from annotations, since it is set already")
				slurmFlags = reCPU.ReplaceAllString(slurmFlags, "")
			}
		} else {
			if reCPU.MatchString(slurmFlags) {
				cpuLimitSetFromFlags = true
			}
		}

		if !isDefaultRAM {
			if reRAM.MatchString(slurmFlags) {
				log.G(ctx).Info("Ignoring --mem flag from annotations, since it is set already")
				slurmFlags = reRAM.ReplaceAllString(slurmFlags, "")
			}
		} else {
			if reRAM.MatchString(slurmFlags) {
				memoryLimitSetFromFlags = true
			}
		}

		sbatchFlagsFromArgo = strings.Split(slurmFlags, " ")

		// Remove empty strings
		var cleanedFlags []string
		for _, flag := range sbatchFlagsFromArgo {
			if flag != "" {
				cleanedFlags = append(cleanedFlags, flag)
			}
		}
		sbatchFlagsFromArgo = cleanedFlags
	}

	// Add resource limits if not set by flags
	if !isDefaultCPU {
		sbatchFlagsFromArgo = append(sbatchFlagsFromArgo, "--cpus-per-task="+strconv.FormatInt(resourceLimits.CPU, 10))
		log.G(ctx).Info("Using CPU limit of " + strconv.FormatInt(resourceLimits.CPU, 10))
	} else {
		log.G(ctx).Info("Using default CPU limit of 1")
		if !cpuLimitSetFromFlags {
			sbatchFlagsFromArgo = append(sbatchFlagsFromArgo, "--cpus-per-task=1")
		}
	}

	if !isDefaultRAM {
		sbatchFlagsFromArgo = append(sbatchFlagsFromArgo, "--mem="+strconv.FormatInt(resourceLimits.Memory/1024/1024, 10))
	} else {
		log.G(ctx).Info("Using default Memory limit of 1MB")
		if !memoryLimitSetFromFlags {
			sbatchFlagsFromArgo = append(sbatchFlagsFromArgo, "--mem=1")
		}
	}

	sbatchFlagsAsString := ""
	for _, slurmFlag := range sbatchFlagsFromArgo {
		sbatchFlagsAsString += "\n#SBATCH " + slurmFlag
	}

	return sbatchFlagsFromArgo, sbatchFlagsAsString, nil
}

// setupTsocksConfiguration sets up Tsocks configuration if enabled
func setupTsocksConfiguration(ctx context.Context, config Config, podUID string) (string, string) {
	prefix := ""
	postfix := ""

	if config.Tsocks {
		log.G(ctx).Debug("--- Adding SSH connection and setting ENVs to use TSOCKS")
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

	return prefix, postfix
}

// addAnnotationBasedEnvVars processes annotations and adds environment variables
func addAnnotationBasedEnvVars(metadata metav1.ObjectMeta, config Config, prefix *string) {
	if podIP, ok := metadata.Annotations["interlink.eu/pod-ip"]; ok {
		*prefix += "\n" + "export POD_IP=" + podIP + "\n"
	}

	if config.Commandprefix != "" {
		*prefix += "\n" + config.Commandprefix
	}

	if wstunnelClientCommands, ok := metadata.Annotations["interlink.eu/wstunnel-client-commands"]; ok {
		*prefix += "\n" + wstunnelClientCommands + "\n"
	}

	if preExecAnnotations, ok := metadata.Annotations["slurm-job.vk.io/pre-exec"]; ok {
		*prefix += "\n" + preExecAnnotations
	}
}

// generateSbatchScript generates the sbatch script content
func generateSbatchScript(config Config, podUID, path, sbatchFlagsAsString, prefix string, fSh *os.File) string {
	return "#!" + config.BashPath +
		"\n#SBATCH --job-name=" + podUID +
		"\n#SBATCH --output=" + path + "/job.out" +
		sbatchFlagsAsString +
		"\n" +
		prefix + " " + fSh.Name() +
		"\n"
}

// getSbatchCommonFunctions returns the common bash functions used in job scripts
func getSbatchCommonFunctions() string {
	return `

####
# Functions
####

# Wait for 60 times 2s if the file exist. The file can be a directory or symlink or anything.
waitFileExist() {
  filePath="$1"
  printf "%s\n" "$(date -Is --utc) Checking if file exists: ${filePath} ..."
  i=1
  iMax=60
  while test "${i}" -le "${iMax}" ; do
	if test -e "${filePath}" ; then
	  printf "%s\n" "$(date -Is --utc) attempt ${i}/${iMax} file found ${filePath}"
	  break
	fi
    printf "%s\n" "$(date -Is --utc) attempt ${i}/${iMax} file not found ${filePath}"
	i=$((i + 1))
    sleep 2
  done
}

runInitCtn() {
  ctn="$1"
  shift
  printf "%s\n" "$(date -Is --utc) Running init container ${ctn}..."
  time ( "$@" ) &> ${workingPath}/init-${ctn}.out
  exitCode="$?"
  printf "%s\n" "${exitCode}" > ${workingPath}/init-${ctn}.status
  waitFileExist "${workingPath}/init-${ctn}.status"
  if test "${exitCode}" != 0 ; then
    printf "%s\n" "$(date -Is --utc) InitContainer ${ctn} failed with status ${exitCode}" >&2
    # InitContainers are fail-fast.
    exit "${exitCode}"
  fi
}

runCtn() {
  ctn="$1"
  shift
  # This subshell below is NOT POSIX shell compatible, it needs for example bash.
  time ( "$@" ) &> ${workingPath}/run-${ctn}.out &
  pid="$!"
  printf "%s\n" "$(date -Is --utc) Running in background ${ctn} pid ${pid}..."
  pidCtns="${pidCtns} ${pid}:${ctn}"
}

waitCtns() {
  # POSIX shell substring test below. Also, container name follows DNS pattern (hyphen alphanumeric, so no ":" inside)
  # pidCtn=12345:container-name-rfc-dns
  # ${pidCtn%:*} => 12345
  # ${pidCtn#*:} => container-name-rfc-dns
  for pidCtn in ${pidCtns} ; do
    pid="${pidCtn%:*}"
    ctn="${pidCtn#*:}"
    printf "%s\n" "$(date -Is --utc) Waiting for container ${ctn} pid ${pid}..."
    wait "${pid}"
    exitCode="$?"
    printf "%s\n" "${exitCode}" > "${workingPath}/run-${ctn}.status"
    printf "%s\n" "$(date -Is --utc) Container ${ctn} pid ${pid} ended with status ${exitCode}."
	waitFileExist "${workingPath}/run-${ctn}.status"
  done
  # Compatibility with jobScript, read the result of conainer .status files
  for filestatus in $(ls *.status) ; do
    exitCode=$(cat "$filestatus")
    test "${highestExitCode}" -lt "${exitCode}" && highestExitCode="${exitCode}"
  done
}

endScript() {
  printf "%s\n" "$(date -Is --utc) End of script, highest exit code ${highestExitCode}..."
  # Deprecated the sleep in favor of checking the status file with waitFileExist (see above).
  #printf "%s\n" "$(date -Is --utc) Sleeping 30s in case of..."
  # For some reason, the status files does not have the time for being written in some HPC, because slurm kills the job too soon.
  #sleep 30

  exit "${highestExitCode}"
}

####
# Main
####

highestExitCode=0

	`
}

// processSingularityCommands processes all singularity commands and generates script content
func processSingularityCommands(ctx context.Context, config Config, commands []SingularityCommand, path string, stringToBeWritten *strings.Builder) error {
	for _, singularityCommand := range commands {
		stringToBeWritten.WriteString("\n")

		if singularityCommand.isInitContainer {
			stringToBeWritten.WriteString("runInitCtn ")
		} else {
			stringToBeWritten.WriteString("runCtn ")
		}
		stringToBeWritten.WriteString(singularityCommand.containerName)
		stringToBeWritten.WriteString(" ")
		stringToBeWritten.WriteString(strings.Join(singularityCommand.singularityCommand, " "))

		if singularityCommand.containerCommand != nil {
			// Case the pod specified a container entrypoint array to override.
			for _, commandEntry := range singularityCommand.containerCommand {
				stringToBeWritten.WriteString(" ")
				fmt.Fprintf(stringToBeWritten, "%q", commandEntry) // Simple quoting
			}
		}
		if singularityCommand.containerArgs != nil {
			// Case the pod specified a container command array to override.
			for _, argsEntry := range singularityCommand.containerArgs {
				stringToBeWritten.WriteString(" ")
				fmt.Fprintf(stringToBeWritten, "%q", argsEntry) // Simple quoting
			}
		}

		// Generate probe scripts if enabled and not an init container
		if config.EnableProbes && !singularityCommand.isInitContainer && (len(singularityCommand.readinessProbes) > 0 || len(singularityCommand.livenessProbes) > 0) {
			if err := processProbeScripts(ctx, config, path, singularityCommand, stringToBeWritten); err != nil {
				return err
			}
		}
	}
	return nil
}

// processProbeScripts processes probe scripts for a container
func processProbeScripts(ctx context.Context, config Config, path string, singularityCommand SingularityCommand, stringToBeWritten *strings.Builder) error {
	// Extract the image name from the singularity command
	var imageName string
	for i, arg := range singularityCommand.singularityCommand {
		if strings.HasPrefix(arg, config.ImagePrefix) || strings.HasPrefix(arg, "/") {
			imageName = arg
			break
		}
		// Look for image after singularity run/exec command
		if (arg == singularityRunCommand || arg == string(ProbeTypeExec)) && i+1 < len(singularityCommand.singularityCommand) {
			// Skip any options and find the image
			for j := i + 1; j < len(singularityCommand.singularityCommand); j++ {
				nextArg := singularityCommand.singularityCommand[j]
				if !strings.HasPrefix(nextArg, "-") && (strings.HasPrefix(nextArg, config.ImagePrefix) || strings.HasPrefix(nextArg, "/")) {
					imageName = nextArg
					break
				}
			}
			break
		}
	}

	if imageName != "" {
		// Store probe metadata for status checking
		err := storeProbeMetadata(path, singularityCommand.containerName, len(singularityCommand.readinessProbes), len(singularityCommand.livenessProbes))
		if err != nil {
			log.G(ctx).Error("Failed to store probe metadata: ", err)
		}

		probeScript := generateProbeScript(ctx, config, singularityCommand.containerName, imageName, singularityCommand.readinessProbes, singularityCommand.livenessProbes)
		stringToBeWritten.WriteString("\n")
		stringToBeWritten.WriteString(probeScript)
	}
	return nil
}

// writeJobScriptContent writes the main job script content
func writeJobScriptContent(
	ctx context.Context,
	config Config,
	path string,
	commands []SingularityCommand,
	fSh *os.File,
	postfix string,
) error {
	var stringToBeWritten strings.Builder

	// Add common bash functions
	sbatchCommonFuncsMacros := getSbatchCommonFunctions()
	stringToBeWritten.WriteString(sbatchCommonFuncsMacros)

	// Adding the workingPath as variable
	stringToBeWritten.WriteString("\nexport workingPath=" + path + "\n")
	stringToBeWritten.WriteString("\nexport SANDBOX=" + path + "\n")

	// Generate probe cleanup script if any probes exist
	if err := addProbeCleanupScript(ctx, config, commands, &stringToBeWritten); err != nil {
		return err
	}

	// Process singularity commands
	if err := processSingularityCommands(ctx, config, commands, path, &stringToBeWritten); err != nil {
		return err
	}

	stringToBeWritten.WriteString("\n" + postfix)

	// Waits for all containers to end, then exit with the highest exit code
	stringToBeWritten.WriteString("\nwaitCtns\nendScript\n\n")

	_, err := fSh.WriteString(stringToBeWritten.String())
	if err != nil {
		log.G(ctx).Error(err)
		return err
	}
	log.G(ctx).Debug("---- Written job.sh file")

	return nil
}

// processMPIFlags handles MPI flags from annotations
func processMPIFlags(metadata metav1.ObjectMeta, commands []SingularityCommand) error {
	if mpiFlags, ok := metadata.Annotations["slurm-job.vk.io/mpi-flags"]; ok {
		if mpiFlags != "true" {
			mpi := append([]string{"mpiexec", "-np", "$SLURM_NTASKS"}, strings.Split(mpiFlags, " ")...)
			for _, singularityCommand := range commands {
				singularityCommand.singularityCommand = append(mpi, singularityCommand.singularityCommand...)
			}
		}
	}
	return nil
}

// writeJobSlurmFile writes the job.slurm file
func writeJobSlurmFile(ctx context.Context, fJob *os.File, sbatchScript string) error {
	log.G(ctx).Debug("--- Writing SLURM sbatch file")

	var jobStringToBeWritten strings.Builder
	jobStringToBeWritten.WriteString(sbatchScript)

	_, err := fJob.WriteString(jobStringToBeWritten.String())
	if err != nil {
		log.G(ctx).Error(err)
		return err
	}

	log.G(ctx).Debug("---- Written job.slurm file")
	return nil
}

// createScriptFiles creates the job.slurm and job.sh files with proper permissions
func createScriptFiles(ctx context.Context, path string) (*os.File, *os.File, error) {
	log.G(ctx).Info("-- Creating file for the Slurm script")

	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		log.G(ctx).Error(err)
		return nil, nil, err
	}
	log.G(ctx).Info("-- Created directory " + path)

	fJob, err := os.Create(path + "/job.slurm")
	if err != nil {
		log.G(ctx).Error("Unable to create file ", path, "/job.slurm")
		log.G(ctx).Error(err)
		return nil, nil, err
	}

	err = os.Chmod(path+"/job.slurm", 0774)
	if err != nil {
		log.G(ctx).Error("Unable to chmod file ", path, "/job.slurm")
		log.G(ctx).Error(err)
		fJob.Close()
		return nil, nil, err
	}
	log.G(ctx).Debug("--- Created with correct permission file ", path, "/job.slurm")

	fSh, err := os.Create(path + "/job.sh")
	if err != nil {
		log.G(ctx).Error("Unable to create file ", path, "/job.sh")
		log.G(ctx).Error(err)
		fJob.Close()
		return nil, nil, err
	}

	err = os.Chmod(path+"/job.sh", 0774)
	if err != nil {
		log.G(ctx).Error("Unable to chmod file ", path, "/job.sh")
		log.G(ctx).Error(err)
		fJob.Close()
		fSh.Close()
		return nil, nil, err
	}
	log.G(ctx).Debug("--- Created with correct permission file ", path, "/job.sh")

	return fJob, fSh, nil
}

// addProbeCleanupScript adds probe cleanup script if any probes exist
func addProbeCleanupScript(_ context.Context, config Config, commands []SingularityCommand, stringToBeWritten *strings.Builder) error {
	// Generate probe cleanup script first if any probes exist
	var hasProbes bool
	for _, singularityCommand := range commands {
		if len(singularityCommand.readinessProbes) > 0 || len(singularityCommand.livenessProbes) > 0 {
			hasProbes = true
			break
		}
	}
	if hasProbes && config.EnableProbes {
		for _, singularityCommand := range commands {
			if len(singularityCommand.readinessProbes) > 0 || len(singularityCommand.livenessProbes) > 0 {
				cleanupScript := generateProbeCleanupScript(singularityCommand.containerName, singularityCommand.readinessProbes, singularityCommand.livenessProbes)
				stringToBeWritten.WriteString(cleanupScript)
				break // Only need one cleanup script
			}
		}
	}
	return nil
}
