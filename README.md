# :information_source: Overview

![Interlink logo](./docs/static/img/interlink_logo.png)

## Introduction

InterLink aims to provide an abstraction for the execution of a Kubernetes pod on any remote resource capable of managing
a Container execution lifecycle. We target to facilitate the development of provider specific plugins, so the resource
providers can leverage the power of virtual kubelet without a black belt in kubernetes internals.

The project consists of two main components:

- __A Kubernetes Virtual Node:__ based on the [VirtualKubelet](https://virtual-kubelet.io/) technology.
Translating request for a kubernetes pod execution into a remote call to the interLink API server.
- __The interLink API server:__ a modular and pluggable REST server where you can create your own Container manager plugin
(called sidecars), or use the existing ones: remote docker execution on a remote host, singularity Container on
a remote SLURM batch system. This repository aims to maintain the SLURM sidecar as a standalone plugin.

The project got inspired by the [KNoC](https://github.com/CARV-ICS-FORTH/knoc) and
[Liqo](https://github.com/liqotech/liqo/tree/master) projects, enhancing that with the implemention a generic API
layer b/w the virtual kubelet component and the provider logic for the container lifecycle management.

## :electron: Usage

### :bangbang: Requirements

- __[Our Kubernetes Virtual Node and the interLink API server](https://github.com/interTwin-eu/interLink)__

- __[The Go programming language](https://go.dev/doc/install)__ (to build binaries)

- __[Docker Engine](https://docs.docker.com/engine/)__ (optional)

Note: if you want a quick start setup (using a Docker container), Go is not necessary

#### :warning: Pods Requirements :warning:

- It is very important for you to remember to set CPU and Memory Limits in your Pod/Deployment YAML, otherwise default resources will be applied; specifically, if you don't set a CPU limit, only 1 CPU will be used for each task, while if you don't set any Memory limit, only 1MB will be used for each task.

- Docker entrypoints are not supported by Singularity. This means you have to manually specify a command to be executed. If you don't, /bin/sh is assumed as the default one. 

The following is a simple example of a Pod with a specified command and limits properly set:
```yaml
    apiVersion: v1
    kind: Pod
    metadata:
    name: test-pod
    annotations:
        slurm-job.knoc.io/flags: "--job-name=test-pod-cfg -t 2800  --ntasks=8 --nodes=1 --mem-per-cpu=2000"
    spec:
    restartPolicy: Never
    containers:
    - image: docker://busybox:latest 
        command: ["echo"]
        args: ["hello world"]
        imagePullPolicy: Always
        name: "hello world"
        resources:
            limits: 
                memory: 100Mi
                cpu: 2
```


### :fast_forward: Quick Start

Just run:

```bash
cd docker && docker compose up -d
```

This way, a docker container with a full SLURM environment will be started. You can find the used SLURM configuration
in /docker/SlurmConfig.yaml. If you want to update the config after you already started it once, run:

```bash
docker compose up -d --build --force-recreate
```

So, the old container will be deleted, the image rebuilt and a new container with the updated config will be deployed.

### :hammer: Building binaries

It is of course possible to use binaries as a standalone application. Just run

```bash
make all
```

and you will be able to find the built slurm-sd binary inside the bin directory. Before executing it, remember to check
if the configuration file is correctly set according to your needs. You can find an example one under examples/config/SlurmConfig.yaml.
Do not forget to set the SLURMCONFIGPATH environment variable to point to your config.

### :gear: A SLURM config example

```yaml
SidecarPort: "4000"
Socket: ""
SbatchPath: "/usr/bin/sbatch"
ScancelPath: "/usr/bin/scancel"
SqueuePath: "/usr/bin/squeue"
SinfoPath: "/usr/bin/sinfo"
CommandPrefix: ""
ImagePrefix: "docker://"
SingularityPath: "singularity"
SingularityPrefix: ""
SingularityDefaultOptions: []
ExportPodData: true
DataRootFolder: ".local/interlink/jobs/"
Namespace: "vk"
Tsocks: false
TsocksPath: "$WORK/tsocks-1.8beta5+ds1/libtsocks.so"
TsocksLoginNode: "login01"
BashPath: /bin/bash
VerboseLogging: true
ErrorsOnlyLogging: false
EnableProbes: true
```

### :pencil2: Annotations

It is possible to specify Annotations when submitting Pods to the K8S cluster. A list of all Annotations follows:
| Annotation    | Description|
|--------------|------------|
| slurm-job.vk.io/singularity-commands | Used to add specific Commands to be executed before the actual SLURM Job starts. It adds Commands on the Singularity exection line, in the SLURM bastch file |
| slurm-job.vk.io/pre-exec | Used to add commands to be executed before the Job starts. It adds a command in the SLURM batch file after the #SBATCH directives |
| slurm-job.vk.io/singularity-mounts | Used to add mountpoints to the Singularity Containers |
| slurm-job.vk.io/singularity-options | Used to specify Singularity arguments |
| slurm-job.vk.io/image-root | Used to specify the root path of the Singularity Image |
| slurm-job.vk.io/flags | Used to specify SLURM flags. These flags will be added to the SLURM script in the form of #SBATCH flag1, #SBATCH flag2, etc |
| slurm-job.vk.io/mpi-flags | Used to prepend "mpiexec -np $SLURM_NTASKS \*flags\*" to the Singularity Execution |
| slurm-job.vk.io/flavor | Used to explicitly select a flavor configuration (e.g., "gpu-nvidia", "high-io") |

**Note**: To specify a custom User ID (UID) for SLURM jobs, use the Kubernetes standard `spec.securityContext.runAsUser` field in your pod specification (see UID Configuration section below).

### :art: Flavor System

The SLURM plugin supports "flavors" - predefined configurations that provide default resource values and SLURM-specific settings. This simplifies pod definitions and ensures consistent resource allocation across jobs.

#### How Flavors Work

Flavors are resolved in the following priority order:
1. **Explicit annotation**: `slurm-job.vk.io/flavor: "flavor-name"`
2. **Auto-detection**: GPU resources automatically select GPU flavors (exact GPU count match preferred)
3. **Default flavor**: Falls back to the flavor specified in `DefaultFlavor` config

#### Configuring Flavors

Add flavors to your `SlurmConfig.yaml`:

```yaml
DefaultFlavor: "default"
Flavors:
  default:
    Name: "default"
    Description: "Standard CPU job (4 cores, 16GB RAM)"
    CPUDefault: 4
    MemoryDefault: "16G"
    SlurmFlags:
      - "--partition=cpu"
      - "--time=01:00:00"

  gpu-nvidia:
    Name: "gpu-nvidia"
    Description: "GPU job with NVIDIA GPU (8 cores, 64GB RAM, 1 GPU)"
    CPUDefault: 8
    MemoryDefault: "64G"
    UID: 2000  # Optional: Set a specific UID for this flavor
    SlurmFlags:
      - "--gres=gpu:1"
      - "--partition=gpu"
      - "--time=04:00:00"

  high-io:
    Name: "high-io"
    Description: "High I/O job (16 cores, 32GB RAM, fast storage)"
    CPUDefault: 16
    MemoryDefault: "32G"
    SlurmFlags:
      - "--partition=fast-io"
      - "--constraint=ssd"
```

#### Flavor Behavior

- **Default Resources**: Flavor CPU/memory defaults apply ONLY when pod doesn't specify resource limits
- **Pod Overrides**: If pod specifies resource limits, those take precedence over flavor defaults
- **SLURM Flag Priority**: Flavor flags < Annotation flags < Pod resource limits
- **Flag Deduplication**: Duplicate flags are automatically removed, with later flags overriding earlier ones

#### Example: Using Flavors

**Example 1: Auto-detected GPU flavor**
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-job
spec:
  containers:
  - name: pytorch
    image: docker://pytorch/pytorch:latest
    resources:
      limits:
        nvidia.com/gpu: 1  # Automatically selects "gpu-nvidia" flavor
```

**Example 2: Explicit flavor selection**
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: io-intensive-job
  annotations:
    slurm-job.vk.io/flavor: "high-io"
spec:
  containers:
  - name: data-processor
    image: docker://myapp:latest
    # Will use high-io flavor's 16 CPU and 32GB RAM defaults
```

**Example 3: Pod resources override flavor defaults**
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: custom-resources
  annotations:
    slurm-job.vk.io/flavor: "default"
spec:
  containers:
  - name: app
    image: docker://myapp:latest
    resources:
      limits:
        cpu: "32"        # Overrides flavor's 4 CPU default
        memory: "128Gi"  # Overrides flavor's 16GB default
```

### :busts_in_silhouette: UID (User ID) Configuration

**RFC**: https://github.com/interlink-hq/interlink-slurm-plugin/discussions/58

The SLURM plugin supports setting a custom User ID (UID) for SLURM jobs. This allows jobs to run as specific users, enabling proper file ownership, permission management, and user attribution for HPC resource accounting.

#### How UID Works

UIDs are resolved with the following priority (highest to lowest):
1. **Pod securityContext**: `spec.securityContext.runAsUser` (Kubernetes standard)
2. **Flavor UID**: Configured in the flavor definition
3. **Default UID**: Configured globally in `DefaultUID`

When a UID is configured, the plugin:
- Adds `--uid=<value>` to the SBATCH script
- Sets file ownership (`chown`) of job directories and scripts to the specified UID
- Ensures the SLURM job runs as the specified user

#### Configuring UID

**Global Configuration** (in `SlurmConfig.yaml`):
```yaml
# Optional: Set a default UID for all jobs
DefaultUID: 1000
```

**Per-Flavor Configuration**:
```yaml
Flavors:
  user-jobs:
    Name: "user-jobs"
    Description: "Jobs for regular users"
    CPUDefault: 8
    MemoryDefault: "32G"
    UID: 1001  # Jobs run as this user
    SlurmFlags:
      - "--partition=users"
```

#### Using UID in Pods

**Example 1: Using Kubernetes securityContext (recommended)**
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: custom-uid-job
spec:
  # Standard Kubernetes securityContext
  securityContext:
    runAsUser: 1001  # SLURM job will run as UID 1001
  containers:
  - name: app
    image: docker://myapp:latest
    resources:
      limits:
        cpu: 4
        memory: 16Gi
```

**Example 2: Using flavor UID**
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: user-job
  annotations:
    slurm-job.vk.io/flavor: "user-jobs"  # Uses UID 1001 from flavor
spec:
  containers:
  - name: computation
    image: docker://scientific-app:latest
```

**Example 3: Complete UID priority example**
```yaml
# Config: DefaultUID: 1000
# Flavor "compute": UID: 2000
# Pod securityContext.runAsUser: 3000
#
# Result: Job runs with UID 3000 (securityContext wins)
apiVersion: v1
kind: Pod
metadata:
  annotations:
    slurm-job.vk.io/flavor: "compute"
spec:
  securityContext:
    runAsUser: 3000  # This takes precedence
  containers:
  - name: app
    image: docker://myapp:latest
```

#### UID Use Cases

- **User Attribution**: Jobs appear in SLURM accounting under the correct user
- **File Ownership**: Output files are owned by the correct user
- **Permission Management**: Jobs respect filesystem permissions
- **Multi-user Environments**: Different users submit jobs through the same Kubernetes cluster
- **HPC Integration**: Integrate with existing HPC user management systems

#### Important Notes

- UID must be a non-negative integer
- Invalid UID values in `securityContext.runAsUser` are ignored (falls back to flavor or default)
- The SLURM cluster must be configured to allow UID specification (plugin typically runs as root)
- File ownership is set via `chown` for job directories (`job.slurm`, `job.sh`)
- The `--uid` flag is added to the SBATCH script as `#SBATCH --uid=<value>`
- This feature is designed for scenarios where the plugin runs as root and needs to impersonate users

### :gear: Explanation of the SLURM Config file

Detailed explanation of the SLURM config file key values. Edit the config file before running the binary or before
building the docker image (`docker compose up -d --build --force-recreate` will recreate and re-run the updated image)
| Key         | Value     |
|--------------|-----------|
| SidecarPort | the sidecar listening port. Sidecar and Interlink will communicate on this port. Set $SIDECARPORT environment variable to specify a custom one |
| Socket | Unix socket path for communication (optional) |
| SbatchPath | path to your Slurm's sbatch binary |
| ScancelPath | path to your Slurm's scancel binary |
| SqueuePath | path to your Slurm's squeue binary |
| SinfoPath | path to your Slurm's sinfo binary |
| CommandPrefix | here you can specify a prefix for the programmatically generated script (for the slurm plugin). Basically, if you want to run anything before the script itself, put it here. |
| ImagePrefix | here you can specify a prefix if you want to prefix the container image name. For example: "docker://". This will do something only if the prefix is not added yet, and if there is no "/" as the first letter of the image name (e.g.: "/root/image.tgz"), which would be an absolute path. Warning: using this field will not allow relative path anymore (e.g.: ./image.tgz and ImagePrefix set to "docker://" will generate "docker://./image.tgz instead of relative path. Use absolute path instead of relative path). Warning2: the the container annotation "slurm-job.vk.io/image-root" is set, this take precedence over ImagePrefix.|
| SingularityPath | path to your Singularity binary |
| SingularityPrefix | prefix to add to Singularity image names |
| SingularityDefaultOptions | array of default options to pass to Singularity commands |
| ExportPodData | Set it to true if you want to export Pod's ConfigMaps and Secrets as mountpoints in your Singularity Container |
| DataRootFolder | Specify where to store the exported ConfigMaps/Secrets locally |
| Namespace | Namespace where Pods in your K8S will be registered |
| Tsocks | true or false values only. Enables or Disables the use of tsocks library to allow proxy networking. Only implemented for the Slurm sidecar at the moment. |
| TsocksPath | path to your tsocks library. |
| TsocksLoginNode | specify an existing node to ssh to. It will be your "window to the external world" |
| BashPath | Path to your Bash shell |
| VerboseLogging | Enable or disable Debug messages on logs. True or False values only |
| ErrorsOnlyLogging | Specify if you want to get errors only on logs. True or false values only |
| EnableProbes | Enable or disable health and readiness probes. True or False values only |
| DefaultUID | Optional default User ID (UID) for all SLURM jobs. Must be a non-negative integer. See [RFC #58](https://github.com/interlink-hq/interlink-slurm-plugin/discussions/58) |
| Flavors | Map of flavor configurations. Each flavor can specify CPUDefault, MemoryDefault, UID, and SlurmFlags. See Flavor System and UID Configuration sections for details |
| DefaultFlavor | Name of the default flavor to use when no explicit flavor is specified and no auto-detection applies |

### :wrench: Environment Variables list

Here's the complete list of every customizable environment variable. When specified, it overwrites the listed key
within the SLURM config file.

| Env         | Value     |
|--------------|-----------|
| SLURMCONFIGPATH | your SLURM config file path. Default is `/etc/interlink/SlurmConfig.yaml` |
| SIDECARPORT | the Sidecar listening port. Docker default is 4000, Slurm default is 4001. |
| SBATCHPATH | path to your Slurm's sbatch binary. Overwrites SbatchPath. |
| SCANCELPATH | path to your Slurm's scancel binary. Overwrites ScancelPath. |
| SHARED_FS | set this env to "true" to save configmaps values inside files directly mounted to Singularity containers instead of using ENVS to create them later |
| CUSTOMKUBECONF | path to a service account kubeconfig |
| TSOCKS | true or false, to use tsocks library allowing proxy networking. Working on Slurm sidecar at the moment. Overwrites Tsocks. |
| TSOCKSPATH | path to your tsocks library. Overwrites TsocksPath. |


### :storage: HostPath Volume Support

The SLURM sidecar plugin has been updated to support Pods that require a HostPath volume. This allows you to run Pods that need access to specific directories on the host machine, which is useful for scenarios where data needs to be shared between the host and the Pod.
It is also possible to specify if the volume is read-only or not, by setting the `readOnly` field in the `volumeMounts` section of the Pod spec.
The following is an example of a Pod that uses a HostPath volume:

```yaml

apiVersion: v1
kind: Pod
metadata:
  name: hostpath-pod
  namespace: interlink
  annotations: {"slurm-job.knoc.io/flags": "--job-name=test-pod"}

spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: vk-slurm-node # specify the virtual node name where the pod should run

  containers:
  - name: hello-world
    image: docker://ghcr.io/grycap/cowsay
    volumeMounts:
    - mountPath: /foo
      name: hostpath-volume
      readOnly: true
    command: ["/bin/cat"]
    args: ["/foo/test"]
    imagePullPolicy: Always
    resources:
      limits:
        memory: "8G"
        cpu: "2"
  
  volumes:
  - name: hostpath-volume
    hostPath:
      path: /data/foo # directory location on host
      type: DirectoryOrCreate # this field is optional

  dnsPolicy: ClusterFirst

  tolerations:
    - key: virtual-node.interlink/no-schedule
      operator: Exists
      effect: NoSchedule
      
```
