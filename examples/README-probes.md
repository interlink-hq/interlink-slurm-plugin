# Readiness and Liveness Probes Support

The interlink-slurm-plugin now supports Kubernetes readiness and liveness probes, allowing containers to be health-checked during execution on SLURM clusters.

## Configuration

To enable probe support, add the following parameter to your `SlurmConfig.yaml`:

```yaml
EnableProbes: true
```

When `EnableProbes` is set to `false` or omitted, the plugin will ignore all probe configurations for backward compatibility.

## Supported Probe Types

### HTTP Probes
- Executes HTTP GET requests against the container
- Uses `curl` inside the Singularity container to perform health checks
- Supports custom paths, ports, hosts, and schemes (HTTP/HTTPS)

### Exec Probes
- Executes custom commands inside the container
- Uses `singularity exec` to run the specified command
- Command exit code determines probe success (0 = success, non-zero = failure)

## How It Works

1. **Translation**: Kubernetes probe specifications are translated to internal probe commands
2. **Execution**: Probes run as background processes alongside the main container
3. **Integration**: Probe scripts are generated and embedded in the SLURM job script
4. **Cleanup**: Probe processes are automatically cleaned up when the job terminates

## Probe Execution Details

- **Readiness Probes**: Determine when a container is ready to receive traffic
- **Liveness Probes**: Determine if a container is healthy and should continue running
- **Init Containers**: Probes are not executed for init containers (only for main containers)
- **Singularity Integration**: All probes execute within the container's Singularity environment

## Example Pod Configuration

See `probe-example.yaml` for a complete example with both HTTP and exec probes:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: probe-test-pod
spec:
  containers:
  - name: web-server
    image: nginx:alpine
    readinessProbe:
      httpGet:
        path: /
        port: 80
        scheme: HTTP
      initialDelaySeconds: 5
      periodSeconds: 10
      timeoutSeconds: 5
      successThreshold: 1
      failureThreshold: 3
    livenessProbe:
      httpGet:
        path: /health
        port: 80
      initialDelaySeconds: 15
      periodSeconds: 20
  - name: app-container
    image: busybox:latest
    readinessProbe:
      exec:
        command:
        - /bin/sh
        - -c
        - "echo 'Ready'"
      initialDelaySeconds: 2
      periodSeconds: 5
    livenessProbe:
      exec:
        command:
        - /bin/sh
        - -c
        - "ps | grep -v grep | grep -q sh"
      initialDelaySeconds: 10
      periodSeconds: 15
```

## Implementation Notes

- Probes run as separate background processes using shell functions
- HTTP probes require `curl` to be available in the container image
- Exec probes can run any command available in the container
- Probe failures are logged with timestamps for debugging
- All probe processes are properly cleaned up on job termination

## Status Integration

Probe status is fully integrated into Kubernetes status reporting:

### Container Readiness
- Containers are marked as `Ready: true` only if:
  1. The container process is running AND
  2. All readiness probes are passing (status = "SUCCESS")
- If any readiness probe fails, the container shows `Ready: false`
- Containers without readiness probes are ready when running (traditional behavior)

### Probe Status Files
The plugin creates status files for monitoring:
- `{workingPath}/readiness-probe-{container}-{index}.status` - Contains: SUCCESS, FAILURE, FAILED_THRESHOLD, or UNKNOWN
- `{workingPath}/readiness-probe-{container}-{index}.timestamp` - Last probe check time
- `{workingPath}/liveness-probe-{container}-{index}.status` - Liveness probe status
- `{workingPath}/liveness-probe-{container}-{index}.timestamp` - Last liveness check time
- `{workingPath}/probe-metadata-{container}.txt` - Probe count metadata

### Status Behavior
- **UNKNOWN**: Probe hasn't started or files don't exist yet
- **SUCCESS**: Probe is passing (consecutive successes ≥ success threshold)
- **FAILURE**: Probe is currently failing but hasn't reached failure threshold
- **FAILED_THRESHOLD**: Probe has failed too many times (liveness probes trigger container restart)

## Backward Compatibility

This feature is fully backward compatible. Existing configurations without the `EnableProbes` parameter will continue to work as before, with probes being ignored and containers marked ready based solely on process status.