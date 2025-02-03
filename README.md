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
SbatchPath: "/usr/bin/sbatch"
ScancelPath: "/usr/bin/scancel"
SqueuePath: "/usr/bin/squeue"
CommandPrefix: ""
ImagePrefix: ""
ExportPodData: true
DataRootFolder: ".local/interlink/jobs/"
Namespace: "vk"
Tsocks: false
TsocksPath: "$WORK/tsocks-1.8beta5+ds1/libtsocks.so"
TsocksLoginNode: "login01"
BashPath: /bin/bash
VerboseLogging: true
ErrorsOnlyLogging: false
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

### :gear: Explanation of the SLURM Config file

Detailed explanation of the SLURM config file key values. Edit the config file before running the binary or before
building the docker image (`docker compose up -d --build --force-recreate` will recreate and re-run the updated image)
| Key         | Value     |
|--------------|-----------|
| SidecarPort | the sidecar listening port. Sidecar and Interlink will communicate on this port. Set $SIDECARPORT environment variable to specify a custom one |
| SbatchPath | path to your Slurm's sbatch binary |
| ScancelPath | path to your Slurm's scancel binary |
| CommandPrefix | here you can specify a prefix for the programmatically generated script (for the slurm plugin). Basically, if you want to run anything before the script itself, put it here. |
| ImagePrefix | here you can specify a prefix if you want to prefix the container image name. For example: "docker://". This will do something only if the prefix is not added yet, and if there is no "/" as the first letter of the image name (e.g.: "/root/image.tgz"), which would be an absolute path. Warning: using this field will not allow relative path anymore (e.g.: ./image.tgz and ImagePrefix set to "docker://" will generate "docker://./image.tgz instead of relative path. Use absolute path instead of relative path). Warning2: the the container annotation "slurm-job.vk.io/image-root" is set, this take precedence over ImagePrefix.|
| ExportPodData | Set it to true if you want to export Pod's ConfigMaps and Secrets as mountpoints in your Singularity Container |
| DataRootFolder | Specify where to store the exported ConfigMaps/Secrets locally |
| Namespace | Namespace where Pods in your K8S will be registered |
| Tsocks | true or false values only. Enables or Disables the use of tsocks library to allow proxy networking. Only implemented for the Slurm sidecar at the moment. |
| TsocksPath | path to your tsocks library. |
| TsocksLoginNode | specify an existing node to ssh to. It will be your "window to the external world" |
| BashPath | Path to your Bash shell |
| VerboseLogging | Enable or disable Debug messages on logs. True or False values only |
| ErrorsOnlyLogging | Specify if you want to get errors only on logs. True or false values only |

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
