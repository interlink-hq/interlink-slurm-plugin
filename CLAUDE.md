# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the InterLink SLURM Plugin, a standalone sidecar that integrates Kubernetes workloads with SLURM batch systems. It's part of the larger InterLink project which provides abstraction for executing Kubernetes pods on remote resources.

Key architecture components:
- **Main sidecar service** (`cmd/main.go`): HTTP server that exposes REST endpoints for pod lifecycle management
- **SLURM plugin package** (`pkg/slurm/`): Core functionality for translating Kubernetes pod operations to SLURM jobs
- **Configuration system**: YAML-based config with environment variable overrides
- **Telemetry integration**: OpenTelemetry tracing support

## Common Development Commands

### Building
```bash
make all              # Build the slurm-sd binary to bin/ directory
make sidecar         # Same as above (alias)
```

### Development Environment
```bash
# Quick start with Docker (includes full SLURM environment)
cd docker && docker compose up -d

# Rebuild after config changes
docker compose up -d --build --force-recreate
```

### Testing
```bash
# Integration tests using Dagger
cd ci && go run main.go call test --interlink-version 0.3.1 --src ../ --plugin-config ../examples/config/SlurmConfig.yaml --manifests ./manifests
```

## Configuration

The SLURM plugin uses a YAML configuration file that can be specified via:
1. `--SlurmConfigpath` flag
2. `SLURMCONFIGPATH` environment variable  
3. Default: `/etc/interlink/SlurmConfig.yaml`

Key configuration settings:
- `SidecarPort`: Plugin listening port (default 4000)
- SLURM binary paths: `SbatchPath`, `ScancelPath`, `SqueuePath`
- Data handling: `ExportPodData`, `DataRootFolder`
- Networking: `Tsocks` for proxy support
- Logging: `VerboseLogging`, `ErrorsOnlyLogging`

Environment variables override config file values (see README.md for complete list).

## Core Plugin Architecture

The plugin operates as an HTTP server with these endpoints:
- `/create`: Submits pods as SLURM jobs
- `/delete`: Cancels SLURM jobs  
- `/status`: Queries job status
- `/getLogs`: Retrieves job logs

**Key files:**
- `pkg/slurm/Create.go`: Pod-to-SLURM job translation and submission
- `pkg/slurm/Delete.go`: Job cancellation logic
- `pkg/slurm/Status.go`: Job status monitoring
- `pkg/slurm/GetLogs.go`: Log retrieval from SLURM
- `pkg/slurm/prepare.go`: ConfigMap/Secret handling and Singularity setup
- `pkg/slurm/func.go`: Configuration loading and shared utilities

## Development Notes

- The plugin translates Kubernetes pods into SLURM batch scripts that run Singularity containers
- Pod annotations control SLURM job parameters (see `slurm-job.vk.io/*` annotations)
- ConfigMaps and Secrets are exported as files when `ExportPodData` is enabled
- Job IDs are tracked in memory and persisted to support restarts
- Telemetry can be enabled with `ENABLE_TRACING=1` environment variable