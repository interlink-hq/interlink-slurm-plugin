# Batch System SDK

`pkg/batchsystem` defines a scheduler-agnostic interface for running
Kubernetes pods on HPC batch systems, plus a small factory used to register and
instantiate backends. SLURM is the only backend implemented today
(`pkg/slurm/adapter.go`); the SDK exists so that additional schedulers (PBS,
LSF, HTCondor, ...) can be added without touching the HTTP layer or the SLURM
code.

## Interface

```go
type BatchSystem interface {
    Initialize(ctx context.Context) error
    Create(ctx context.Context, pod commonIL.RetrievedPodData) (CreateResult, error)
    Delete(ctx context.Context, pod *v1.Pod) error
    Status(ctx context.Context, pods []*v1.Pod) ([]commonIL.PodStatus, error)
    GetLogs(ctx context.Context, pod *v1.Pod, containerName string, opts LogOptions) ([]byte, error)
    GetSystemInfo(ctx context.Context) (string, error)
}
```

`commonIL` is `github.com/interlink-hq/interlink/pkg/interlink`, the types
shared with the interLink API server. The methods map one-to-one onto the
plugin HTTP endpoints (`/create`, `/delete`, `/status`, `/getLogs`,
`/system-info`).

| Type | Purpose |
|------|---------|
| `CreateResult` | `{PodUID, PodJID}` returned by `Create` (same JSON shape the API server expects from `/create`). |
| `LogOptions` | `Tail`, `LimitBytes`, `Follow`, `Previous`, `Timestamps`. Only `Tail`/`LimitBytes` are honoured by the SLURM backend. |

## Factory

Backends register a `Constructor` with the process-wide `DefaultFactory` from
their package `init()`; importing the backend package is enough to make it
available:

```go
import (
    "github.com/intertwin-eu/interlink-slurm-plugin/pkg/batchsystem"
    _ "github.com/intertwin-eu/interlink-slurm-plugin/pkg/slurm" // registers "slurm"
)

bs, err := batchsystem.CreateBatchSystem(ctx, batchsystem.FactoryConfig{
    Type:   batchsystem.BatchSystemSLURM,
    Config: slurmConfig, // slurm.SlurmConfig or *slurm.SlurmConfig
})
if err != nil {
    return err
}
if err := bs.Initialize(ctx); err != nil {
    return err
}
```

`cmd/main.go` builds the SLURM backend this way and mounts the existing HTTP
handlers from `(*slurm.SlurmBatchSystem).Handler()`.

A custom `Factory` (via `NewFactory()`) can be used instead of the default one,
e.g. in tests.

## SLURM backend

`pkg/slurm/adapter.go` wraps `SidecarHandler` and calls the same functions the
HTTP handlers use:

| `BatchSystem` method | `SidecarHandler` code path |
|----------------------|----------------------------|
| `Initialize` | `CreateDirectories()` + `LoadJIDs()` |
| `Create` | `submitPod()` (also used by `SubmitHandler`) |
| `Delete` | `deletePod()` (also used by `StopHandler`) |
| `Status` | `podStatuses()` (also used by `StatusHandler`) |
| `GetLogs` | `resolveLogDir()` + `trimLogOutput()` (shared with `GetLogsHandler`) |
| `GetSystemInfo` | `getSinfoSummary()` |

Behaviour is therefore identical regardless of whether a request arrives over
HTTP or through the interface.

## Adding a backend

1. Create `pkg/<name>/<name>.go` with a type implementing `batchsystem.BatchSystem`.
   Add `var _ batchsystem.BatchSystem = (*YourType)(nil)` to get a compile-time check.
2. Provide a constructor matching `batchsystem.Constructor`:
   `func New<Name>BatchSystem(ctx context.Context, config interface{}) (batchsystem.BatchSystem, error)`.
   Type-assert `config` to your own configuration struct and return a clear
   error on mismatch.
3. Add a `BatchSystemType` constant in `factory.go`
   (e.g. `BatchSystemPBS BatchSystemType = "pbs"`).
4. Register from `init()`:
   `batchsystem.RegisterDefault(batchsystem.BatchSystemPBS, NewPBSBatchSystem)`.
5. Blank-import the package where the factory is used (`cmd/main.go`).
6. Add tests next to the implementation; `pkg/slurm/adapter_test.go` shows how
   to exercise a backend with fake scheduler binaries.
