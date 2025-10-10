# Batch System SDK

This package provides an SDK-like abstraction layer for supporting multiple batch scheduling systems in the InterLink plugin. Instead of being limited to SLURM, the plugin can now work with any batch system that implements the `BatchSystem` interface.

## Architecture Overview

The architecture follows a factory pattern with a common interface:

```
┌─────────────────────────────────────────────┐
│           InterLink Plugin                  │
│                                             │
│  ┌───────────────────────────────────────┐ │
│  │   BatchSystem Interface               │ │
│  │   - Create()                          │ │
│  │   - Delete()                          │ │
│  │   - Status()                          │ │
│  │   - GetLogs()                         │ │
│  │   - GetSystemInfo()                   │ │
│  └───────────────────────────────────────┘ │
│              ▲         ▲         ▲          │
│              │         │         │          │
│      ┌───────┴─┐   ┌──┴──┐   ┌──┴────┐    │
│      │  SLURM  │   │ PBS │   │  LSF  │    │
│      │  Impl   │   │Impl │   │ Impl  │    │
│      └─────────┘   └─────┘   └───────┘    │
└─────────────────────────────────────────────┘
```

## Directory Structure

```
pkg/
├── batchsystem/          # SDK core
│   ├── interface.go      # BatchSystem interface definition
│   ├── types.go          # Common types (CreateResult, LogOptions, etc.)
│   ├── factory.go        # Factory for creating batch system instances
│   └── README.md         # This file
├── slurm/                # SLURM implementation
│   ├── adapter.go        # SLURM adapter implementing BatchSystem
│   ├── Create.go         # Existing SLURM create logic
│   ├── Delete.go         # Existing SLURM delete logic
│   ├── Status.go         # Existing SLURM status logic
│   └── ...
└── pbs/                  # PBS/Torque implementation (stub)
    └── pbs.go            # PBS implementation (to be completed)
```

## How to Add a New Batch System

To add support for a new batch system (e.g., LSF, HTCondor):

### 1. Create a new package

```bash
mkdir pkg/lsf
```

### 2. Implement the BatchSystem interface

Create `pkg/lsf/lsf.go`:

```go
package lsf

import (
    "context"
    "github.com/intertwin-eu/interlink-slurm-plugin/pkg/batchsystem"
    commonIL "github.com/intertwin-eu/interlink/pkg/interlink"
    v1 "k8s.io/api/core/v1"
)

type LSFBatchSystem struct {
    config LSFConfig
    jobs   map[string]*batchsystem.JobMetadata
}

func NewLSFBatchSystem(ctx context.Context, config interface{}) (batchsystem.BatchSystem, error) {
    // Implementation
}

// Implement all BatchSystem interface methods:
func (l *LSFBatchSystem) Create(ctx context.Context, pod commonIL.RetrievedPodData) (batchsystem.CreateResult, error) {
    // Translate pod to LSF bsub script
    // Submit job
    // Return job ID
}

func (l *LSFBatchSystem) Delete(ctx context.Context, pod *v1.Pod) error {
    // Use bkill to cancel job
}

func (l *LSFBatchSystem) Status(ctx context.Context, pods []*v1.Pod) ([]commonIL.PodStatus, error) {
    // Query bjobs for status
    // Map LSF states to Kubernetes states
}

// ... implement other methods
```

### 3. Register with the factory

Add to your package's `init()` function:

```go
func init() {
    batchsystem.RegisterDefault(batchsystem.BatchSystemLSF, NewLSFBatchSystem)
}
```

### 4. Update the factory constants

In `pkg/batchsystem/factory.go`, add:

```go
const (
    // ... existing constants
    BatchSystemLSF BatchSystemType = "lsf"
)
```

### 5. Import in main.go

```go
import (
    _ "github.com/intertwin-eu/interlink-slurm-plugin/pkg/lsf"  // Register LSF
)
```

## Configuration

The batch system type is specified in the configuration file:

```yaml
# For SLURM
batchSystemType: "slurm"
SbatchPath: "/usr/bin/sbatch"
ScancelPath: "/usr/bin/scancel"
# ... SLURM-specific config

# For PBS
batchSystemType: "pbs"
QsubPath: "/usr/bin/qsub"
QdelPath: "/usr/bin/qdel"
# ... PBS-specific config
```

## Usage Example

```go
import (
    "context"
    "github.com/intertwin-eu/interlink-slurm-plugin/pkg/batchsystem"
    _ "github.com/intertwin-eu/interlink-slurm-plugin/pkg/slurm"  // Register SLURM
    _ "github.com/intertwin-eu/interlink-slurm-plugin/pkg/pbs"   // Register PBS
)

func main() {
    ctx := context.Background()

    // Create a batch system instance based on configuration
    bs, err := batchsystem.CreateBatchSystem(ctx, batchsystem.FactoryConfig{
        Type:   batchsystem.BatchSystemSLURM,
        Config: slurmConfig,
    })
    if err != nil {
        panic(err)
    }

    // Initialize
    if err := bs.Initialize(ctx); err != nil {
        panic(err)
    }

    // Use the batch system
    result, err := bs.Create(ctx, podData)
    // ...
}
```

## Benefits

1. **Extensibility**: Add new batch systems without modifying existing code
2. **Maintainability**: Each batch system is isolated in its own package
3. **Testability**: Mock implementations for testing
4. **Flexibility**: Switch batch systems via configuration
5. **Backward Compatibility**: Existing SLURM functionality preserved

## Current Implementations

- **SLURM**: Fully implemented (wrapped existing code)
- **PBS/Torque**: Stub implementation (to be completed)
- **LSF**: Not implemented
- **HTCondor**: Not implemented

## Common Types

### CreateResult
```go
type CreateResult struct {
    PodUID string  // Kubernetes pod UID
    PodJID string  // Batch system job ID
}
```

### LogOptions
```go
type LogOptions struct {
    Tail       int
    Follow     bool
    Previous   bool
    Timestamps bool
}
```

### JobMetadata
```go
type JobMetadata struct {
    JID       string
    PodUID    string
    PodName   string
    Namespace string
    StartTime time.Time
    EndTime   time.Time
}
```

## Future Enhancements

- [ ] Complete PBS implementation
- [ ] Add LSF support
- [ ] Add HTCondor support
- [ ] Add validation for batch system configurations
- [ ] Add metrics/telemetry per batch system
- [ ] Support for custom/external batch systems via plugins
