# Pod State Diagram - Quick Reference

## SLURM to Kubernetes State Mapping

| SLURM State | Full Name | Kubernetes State | Container Ready | Notes |
|-------------|-----------|------------------|-----------------|-------|
| **PD** | Pending | `Waiting` | `false` | Awaiting resources |
| **S** | Suspended | `Waiting` | `false` | Temporarily halted |
| **R** | Running | `Running` | Probe-dependent* | Actively executing |
| **CG** | Completing | `Running` | Probe-dependent* | Finishing up |
| **CD** | Completed | `Terminated` | `false` | Success (exit 0) |
| **F** | Failed | `Terminated` | `false` | Failed/abnormal exit |
| **PR** | Preempted | `Terminated` | `false` | Interrupted by higher priority |
| **ST** | Stopped | `Terminated` | `false` | Stopped/timeout |
| **Other** | Unknown | `Terminated` | `false` | Unrecognized state |
| **No JID** | Not found | `{}` | `false` | Pod not submitted yet |

*\* Probe-dependent: `Ready: true` only if all readiness probes are `SUCCESS`*

## State Flow Diagram

```
┌─────────────┐
│ Pod Created │
└─────┬───────┘
      │
      ▼
┌─────────────────┐
│ SLURM Job       │
│ Submitted       │
└─────┬───────────┘
      │
      ▼
┌─────────────────┐    ┌──────────────┐
│ PD/S            │    │ Waiting      │
│ (Queued/Halted) │───▶│ Ready: false │
└─────────────────┘    └──────────────┘
      │
      ▼
┌─────────────────┐    ┌──────────────┐    ┌─────────────────┐
│ R/CG            │    │ Running      │    │ Readiness       │
│ (Executing)     │───▶│ Ready: ???   │───▶│ Probe Check     │
└─────────────────┘    └──────────────┘    └─────┬───────────┘
      │                                          │
      │                                          ▼
      │                                  ┌───────────────┐
      │                                  │ All SUCCESS?  │
      │                                  └─────┬─────────┘
      │                                    Yes │ │ No
      │                                        ▼ ▼
      │                                  ┌──────────────┐
      │                                  │ Ready: true  │
      │                                  │ Ready: false │
      │                                  └──────────────┘
      ▼                                                          
┌─────────────────┐    ┌──────────────┐
│ CD/F/PR/ST      │    │ Terminated   │
│ (Final States)  │───▶│ Ready: false │
└─────────────────┘    └──────────────┘
```

## Readiness Logic Detail

### Without Probes
```
Running State → Ready: true
```

### With Readiness Probes
```
Running State → Check All Probes → Ready: (all == "SUCCESS")
```

### Probe Status Values
- ✅ **SUCCESS**: All checks passing
- ❌ **FAILURE**: Currently failing (retrying)  
- ⛔ **FAILED_THRESHOLD**: Too many failures
- ❓ **UNKNOWN**: Not started/no status file

## Timeline Example

```
Time →  0s    10s   20s   30s   40s   50s   60s
        │     │     │     │     │     │     │
State   PD ───▶ R ──▶ R ──▶ R ──▶ CG ─▶ CD  │
        │     │     │     │     │     │     │
K8s     Waiting   Running Running Running   Terminated
        │     │     │     │     │     │     │
Ready   false │   true  true  true  │   false
              │                     │
        StartedAt                FinishedAt    
        recorded                recorded
```

## Key Files Created

### State Tracking
- `{workingPath}/StartedAt.time` - When job started running
- `{workingPath}/FinishedAt.time` - When job finished
- `{workingPath}/JobID.jid` - SLURM job ID
- `{workingPath}/PodUID.uid` - Kubernetes pod UID

### Container Status  
- `{workingPath}/run-{container}.status` - Container exit code
- `{workingPath}/init-{container}.status` - Init container exit code

### Probe Status (if enabled)
- `{workingPath}/readiness-probe-{container}-{index}.status`
- `{workingPath}/readiness-probe-{container}-{index}.timestamp`
- `{workingPath}/probe-metadata-{container}.txt`