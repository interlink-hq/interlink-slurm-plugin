package slurm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// translateKubernetesProbes converts Kubernetes probe specifications to internal ProbeCommand format
func translateKubernetesProbes(ctx context.Context, container v1.Container) ([]ProbeCommand, []ProbeCommand) {
	var readinessProbes, livenessProbes []ProbeCommand
	span := trace.SpanFromContext(ctx)

	// Handle readiness probe
	if container.ReadinessProbe != nil {
		probe := translateSingleProbe(ctx, container.ReadinessProbe)
		if probe != nil {
			readinessProbes = append(readinessProbes, *probe)
			span.AddEvent("Translated readiness probe for container " + container.Name)
		}
	}

	// Handle liveness probe
	if container.LivenessProbe != nil {
		probe := translateSingleProbe(ctx, container.LivenessProbe)
		if probe != nil {
			livenessProbes = append(livenessProbes, *probe)
			span.AddEvent("Translated liveness probe for container " + container.Name)
		}
	}

	return readinessProbes, livenessProbes
}

// translateSingleProbe converts a single Kubernetes probe to internal format
func translateSingleProbe(ctx context.Context, k8sProbe *v1.Probe) *ProbeCommand {
	if k8sProbe == nil {
		return nil
	}

	probe := &ProbeCommand{
		InitialDelaySeconds: k8sProbe.InitialDelaySeconds,
		PeriodSeconds:       k8sProbe.PeriodSeconds,
		TimeoutSeconds:      k8sProbe.TimeoutSeconds,
		SuccessThreshold:    k8sProbe.SuccessThreshold,
		FailureThreshold:    k8sProbe.FailureThreshold,
	}

	// Set defaults if not specified
	if probe.PeriodSeconds == 0 {
		probe.PeriodSeconds = 10
	}
	if probe.TimeoutSeconds == 0 {
		probe.TimeoutSeconds = 1
	}
	if probe.SuccessThreshold == 0 {
		probe.SuccessThreshold = 1
	}
	if probe.FailureThreshold == 0 {
		probe.FailureThreshold = 3
	}

	// Translate HTTP probe
	if k8sProbe.HTTPGet != nil {
		probe.Type = ProbeTypeHTTP
		probe.HTTPGetAction = &HTTPGetAction{
			Path:   k8sProbe.HTTPGet.Path,
			Port:   k8sProbe.HTTPGet.Port.IntVal,
			Host:   k8sProbe.HTTPGet.Host,
			Scheme: string(k8sProbe.HTTPGet.Scheme),
		}

		// Set defaults
		if probe.HTTPGetAction.Scheme == "" {
			probe.HTTPGetAction.Scheme = "HTTP"
		}
		if probe.HTTPGetAction.Path == "" {
			probe.HTTPGetAction.Path = "/"
		}

		return probe
	}

	// Translate Exec probe
	if k8sProbe.Exec != nil {
		probe.Type = ProbeTypeExec
		probe.ExecAction = &ExecAction{
			Command: k8sProbe.Exec.Command,
		}
		return probe
	}

	log.G(ctx).Warning("Unsupported probe type (only HTTP and Exec are supported)")
	return nil
}

// generateProbeScript generates the shell script commands for executing probes
func generateProbeScript(ctx context.Context, config SlurmConfig, containerName string, imageName string, readinessProbes []ProbeCommand, livenessProbes []ProbeCommand) string {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("Generating probe script for container " + containerName)

	if len(readinessProbes) == 0 && len(livenessProbes) == 0 {
		return ""
	}

	var scriptBuilder strings.Builder

	// Function definitions for probe execution
	scriptBuilder.WriteString(`
# Probe execution functions
executeHTTPProbe() {
    local scheme="$1"
    local host="$2"
    local port="$3"
    local path="$4"
    local timeout="$5"
    local container_name="$6"
    
    if [ -z "$host" ] || [ "$host" = "localhost" ] || [ "$host" = "127.0.0.1" ]; then
        host="localhost"
    fi
    
    url="${scheme,,}://${host}:${port}${path}"
    
    # Use curl outside the container
    `)
	scriptBuilder.WriteString(fmt.Sprintf(` timeout "${timeout}" curl -f -s "$url" &> /dev/null
    return $?
}

executeExecProbe() {
    local timeout="$1"
    local container_name="$2"
    shift 2
    local command=("$@")
    
    # Use singularity exec to run the command inside the container
    `, imageName))
	scriptBuilder.WriteString(fmt.Sprintf(`"%s" exec`, config.SingularityPath))
	for _, opt := range config.SingularityDefaultOptions {
		scriptBuilder.WriteString(fmt.Sprintf(` "%s"`, opt))
	}
	scriptBuilder.WriteString(fmt.Sprintf(` "%s" timeout "${timeout}" "${command[@]}"
    return $?
}

runProbe() {
    local probe_type="$1"
    local container_name="$2"
    local initial_delay="$3"
    local period="$4"
    local timeout="$5"
    local success_threshold="$6"
    local failure_threshold="$7"
    local probe_name="$8"
    local probe_index="$9"
    shift 9
    local probe_args=("$@")
    
    local probe_status_file="${workingPath}/${probe_name}-probe-${container_name}-${probe_index}.status"
    local probe_timestamp_file="${workingPath}/${probe_name}-probe-${container_name}-${probe_index}.timestamp"
    
    printf "%%s\n" "$(date -Is --utc) Starting ${probe_name} probe for container ${container_name}..."
    
    # Initialize probe status as unknown
    echo "UNKNOWN" > "$probe_status_file"
    date -Is --utc > "$probe_timestamp_file"
    
    # Initial delay
    if [ "$initial_delay" -gt 0 ]; then
        printf "%%s\n" "$(date -Is --utc) Waiting ${initial_delay}s before starting ${probe_name} probe..."
        sleep "$initial_delay"
    fi
    
    local consecutive_successes=0
    local consecutive_failures=0
    local probe_ready=false
    
    while true; do
        # Update timestamp before each probe attempt
        date -Is --utc > "$probe_timestamp_file"
        
        if [ "$probe_type" = "http" ]; then
            executeHTTPProbe "${probe_args[@]}" "$container_name"
        elif [ "$probe_type" = "exec" ]; then
            executeExecProbe "$timeout" "$container_name" "${probe_args[@]}"
        fi
        
        local exit_code=$?
        
        if [ $exit_code -eq 0 ]; then
            consecutive_successes=$((consecutive_successes + 1))
            consecutive_failures=0
            printf "%%s\n" "$(date -Is --utc) ${probe_name} probe succeeded for ${container_name} (${consecutive_successes}/${success_threshold})"
            
            if [ $consecutive_successes -ge $success_threshold ] && [ "$probe_ready" = false ]; then
                printf "%%s\n" "$(date -Is --utc) ${probe_name} probe successful for ${container_name}"
                echo "SUCCESS" > "$probe_status_file"
                probe_ready=true
            elif [ "$probe_ready" = true ]; then
                # Keep updating status for already successful probes
                echo "SUCCESS" > "$probe_status_file"
            fi
        else
            consecutive_failures=$((consecutive_failures + 1))
            consecutive_successes=0
            printf "%%s\n" "$(date -Is --utc) ${probe_name} probe failed for ${container_name} (${consecutive_failures}/${failure_threshold})"
            
            # Always write failure status immediately
            echo "FAILURE" > "$probe_status_file"
            probe_ready=false
            
            if [ $consecutive_failures -ge $failure_threshold ]; then
                printf "%%s\n" "$(date -Is --utc) ${probe_name} probe failed for ${container_name} after ${failure_threshold} attempts" >&2
                echo "FAILED_THRESHOLD" > "$probe_status_file"
                return 1
            fi
        fi
        
        sleep "$period"
    done
    
    return 0
}

`, imageName))

	// Generate readiness probe calls
	for i, probe := range readinessProbes {
		probeArgs := buildProbeArgs(probe)
		containerVarName := strings.ReplaceAll(containerName, "-", "_")
		scriptBuilder.WriteString(fmt.Sprintf(`
# Readiness probe %d for %s
runProbe "%s" "%s" %d %d %d %d %d "readiness" %d %s &
READINESS_PROBE_%s_%d_PID=$!
`, i, containerName, probe.Type, containerName, probe.InitialDelaySeconds, probe.PeriodSeconds,
			probe.TimeoutSeconds, probe.SuccessThreshold, probe.FailureThreshold, i, probeArgs, containerVarName, i))
	}

	// Generate liveness probe calls
	for i, probe := range livenessProbes {
		probeArgs := buildProbeArgs(probe)
		containerVarName := strings.ReplaceAll(containerName, "-", "_")
		scriptBuilder.WriteString(fmt.Sprintf(`
# Liveness probe %d for %s
runProbe "%s" "%s" %d %d %d %d %d "liveness" %d %s &
LIVENESS_PROBE_%s_%d_PID=$!
`, i, containerName, probe.Type, containerName, probe.InitialDelaySeconds, probe.PeriodSeconds,
			probe.TimeoutSeconds, probe.SuccessThreshold, probe.FailureThreshold, i, probeArgs, containerVarName, i))
	}

	span.SetAttributes(
		attribute.String("probes.container.name", containerName),
		attribute.Int("probes.readiness.count", len(readinessProbes)),
		attribute.Int("probes.liveness.count", len(livenessProbes)),
	)

	return scriptBuilder.String()
}

// buildProbeArgs constructs the argument string for probe execution
func buildProbeArgs(probe ProbeCommand) string {
	switch probe.Type {
	case ProbeTypeHTTP:
		return fmt.Sprintf(`"%s" "%s" %d "%s"`,
			probe.HTTPGetAction.Scheme,
			probe.HTTPGetAction.Host,
			probe.HTTPGetAction.Port,
			probe.HTTPGetAction.Path)
	case ProbeTypeExec:
		args := make([]string, len(probe.ExecAction.Command))
		for i, cmd := range probe.ExecAction.Command {
			args[i] = fmt.Sprintf(`"%s"`, cmd)
		}
		return strings.Join(args, " ")
	default:
		return ""
	}
}

// generateProbeCleanupScript generates cleanup commands for probe processes
func generateProbeCleanupScript(containerName string, readinessProbes []ProbeCommand, livenessProbes []ProbeCommand) string {
	if len(readinessProbes) == 0 && len(livenessProbes) == 0 {
		return ""
	}

	var scriptBuilder strings.Builder
	scriptBuilder.WriteString(`
# Cleanup probe processes
cleanup_probes() {
    printf "%s\n" "$(date -Is --utc) Cleaning up probe processes..."
`)

	containerVarName := strings.ReplaceAll(containerName, "-", "_")
	
	// Kill readiness probes
	for i := range readinessProbes {
		scriptBuilder.WriteString(fmt.Sprintf(`    if [ ! -z "$READINESS_PROBE_%s_%d_PID" ]; then
        kill $READINESS_PROBE_%s_%d_PID 2>/dev/null || true
    fi
`, containerVarName, i, containerVarName, i))
	}

	// Kill liveness probes
	for i := range livenessProbes {
		scriptBuilder.WriteString(fmt.Sprintf(`    if [ ! -z "$LIVENESS_PROBE_%s_%d_PID" ]; then
        kill $LIVENESS_PROBE_%s_%d_PID 2>/dev/null || true
    fi
`, containerVarName, i, containerVarName, i))
	}

	scriptBuilder.WriteString(`}

# Set up trap to cleanup probes on exit
trap cleanup_probes EXIT
`)

	return scriptBuilder.String()
}

// ProbeStatus represents the status of a single probe
type ProbeStatus struct {
	Type      ProbeType
	Status    string // SUCCESS, FAILURE, FAILED_THRESHOLD, UNKNOWN
	Timestamp time.Time
}

// getProbeStatus reads the status of a specific probe from its status file
func getProbeStatus(ctx context.Context, workingPath, probeType, containerName string, probeIndex int) (*ProbeStatus, error) {
	statusFilePath := fmt.Sprintf("%s/%s-probe-%s-%d.status", workingPath, probeType, containerName, probeIndex)
	timestampFilePath := fmt.Sprintf("%s/%s-probe-%s-%d.timestamp", workingPath, probeType, containerName, probeIndex)

	// Read status
	statusBytes, err := os.ReadFile(statusFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Probe file doesn't exist, probe not configured or not started yet
			return &ProbeStatus{
				Type:      ProbeType(probeType),
				Status:    "UNKNOWN",
				Timestamp: time.Now(),
			}, nil
		}
		return nil, fmt.Errorf("failed to read probe status file %s: %w", statusFilePath, err)
	}

	// Read timestamp
	var timestamp time.Time
	timestampBytes, err := os.ReadFile(timestampFilePath)
	if err != nil {
		// If timestamp file doesn't exist, use current time
		timestamp = time.Now()
		log.G(ctx).Debug("Timestamp file not found for probe, using current time: ", timestampFilePath)
	} else {
		timestamp, err = time.Parse(time.RFC3339, strings.TrimSpace(string(timestampBytes)))
		if err != nil {
			log.G(ctx).Warning("Failed to parse probe timestamp, using current time: ", err)
			timestamp = time.Now()
		}
	}

	return &ProbeStatus{
		Type:      ProbeType(probeType),
		Status:    strings.TrimSpace(string(statusBytes)),
		Timestamp: timestamp,
	}, nil
}

// checkContainerReadiness evaluates if a container is ready based on its readiness probes
func checkContainerReadiness(ctx context.Context, config SlurmConfig, workingPath, containerName string, readinessProbeCount int) bool {
	if !config.EnableProbes || readinessProbeCount == 0 {
		// No readiness probes configured, container is ready if running
		return true
	}

	span := trace.SpanFromContext(ctx)
	allProbesSuccessful := true

	for i := 0; i < readinessProbeCount; i++ {
		probeStatus, err := getProbeStatus(ctx, workingPath, "readiness", containerName, i)
		if err != nil {
			log.G(ctx).Error("Failed to check readiness probe status: ", err)
			allProbesSuccessful = false
			continue
		}

		span.SetAttributes(attribute.String(fmt.Sprintf("readiness.probe.%d.status", i), probeStatus.Status))

		if probeStatus.Status != "SUCCESS" {
			allProbesSuccessful = false
			log.G(ctx).Debugf("Readiness probe %d for container %s is not successful: %s", i, containerName, probeStatus.Status)
		}
	}

	span.SetAttributes(attribute.Bool("container.ready", allProbesSuccessful))
	return allProbesSuccessful
}

// checkContainerLiveness evaluates if a container is alive based on its liveness probes
func checkContainerLiveness(ctx context.Context, config SlurmConfig, workingPath, containerName string, livenessProbeCount int) bool {
	if !config.EnableProbes || livenessProbeCount == 0 {
		// No liveness probes configured, container is alive if running
		return true
	}

	span := trace.SpanFromContext(ctx)
	allProbesSuccessful := true

	for i := 0; i < livenessProbeCount; i++ {
		probeStatus, err := getProbeStatus(ctx, workingPath, "liveness", containerName, i)
		if err != nil {
			log.G(ctx).Error("Failed to check liveness probe status: ", err)
			allProbesSuccessful = false
			continue
		}

		span.SetAttributes(attribute.String(fmt.Sprintf("liveness.probe.%d.status", i), probeStatus.Status))

		// For liveness probes, FAILED_THRESHOLD means the container should be considered dead
		if probeStatus.Status == "FAILED_THRESHOLD" {
			allProbesSuccessful = false
			log.G(ctx).Warningf("Liveness probe %d for container %s has failed threshold: %s", i, containerName, probeStatus.Status)
		}
	}

	span.SetAttributes(attribute.Bool("container.alive", allProbesSuccessful))
	return allProbesSuccessful
}

// storeProbeMetadata saves probe count information for later status checking
func storeProbeMetadata(workingPath, containerName string, readinessProbeCount, livenessProbeCount int) error {
	metadataFile := fmt.Sprintf("%s/probe-metadata-%s.txt", workingPath, containerName)
	content := fmt.Sprintf("readiness:%d\nliveness:%d", readinessProbeCount, livenessProbeCount)
	return os.WriteFile(metadataFile, []byte(content), 0644)
}

// loadProbeMetadata loads probe count information for status checking
func loadProbeMetadata(workingPath, containerName string) (readinessCount, livenessCount int, err error) {
	metadataFile := fmt.Sprintf("%s/probe-metadata-%s.txt", workingPath, containerName)
	content, err := os.ReadFile(metadataFile)
	if err != nil {
		if os.IsNotExist(err) {
			// No probe metadata file means no probes configured
			return 0, 0, nil
		}
		return 0, 0, err
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}

		var count int
		if _, err := fmt.Sscanf(parts[1], "%d", &count); err != nil {
			continue
		}

		switch parts[0] {
		case "readiness":
			readinessCount = count
		case "liveness":
			livenessCount = count
		}
	}

	return readinessCount, livenessCount, nil
}
