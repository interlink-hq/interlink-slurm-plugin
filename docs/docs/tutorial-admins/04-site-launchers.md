---
sidebar_position: 4
---

# Multi-pod jobs and site launchers

The pods of one distributed job (a Kubeflow Trainer TrainJob, a KubeRay RayCluster, a
JobSet) can start as **one** Slurm job: all of them together on N nodes, or none.

## Turning it on

Set `GangSchedulingEnabled: true` in the plugin configuration, or
`GANGSCHEDULINGENABLED=true` in its environment. The plugin then lists the capabilities
`pod-group` and `pod-group-create` in its ping answer, so a caller knows it can send
groups, pod by pod or whole. Off (the default), `/createPodGroup` refuses every group
with 400 and `/create` makes one job of every pod.

## How a group arrives: `POST /createPodGroup`

A caller that holds the whole group sends every pod in one call, each pod as `/create`
receives it:

```json
{"groupUID": "<workload UID>", "pods": [...]}
```

The plugin writes every pod's files as `/create` does, then submits one
`sbatch --nodes=<number of pods>` named after `groupUID`. Ranks follow the order of the
pods; rank 0 is the head. Every rank gets one node of the same
shape, sized for the largest CPU, memory and GPU request of any pod.

The answer lists one `{"PodUID": ..., "PodJID": ...}` per pod, in the order of the
request, all with the group's job ID. Status, logs and delete stay per pod: each pod
reports its own containers' exit codes, a rank that fails shows on its own pod while the
others keep running, and the job is cancelled once, when the last pod of the group is
deleted.

The same call sent again for the same pods answers with the same job, without a second
`sbatch`. A group sent again under the same `groupUID` with other pods is submitted
afresh once the old job has ended, and refused with 503 while it runs. A refusal by
`sbatch` answers 503 when trying again later can work (ssh to the site failed, a limit on
submitted jobs, a busy controller) and 400 otherwise (an invalid account, partition or
QOS), with `sbatch`'s text; the pods' files are removed.

## Which pods form a gang

A group can also reach the plugin pod by pod, through `/create`, as the virtual kubelet
sends every pod; the virtual kubelet knows nothing about groups. The plugin reads
Kueue's pod-group keys, whoever writes them (Kueue, a policy engine, the user):

| Key | Kind | Meaning |
|---|---|---|
| `kueue.x-k8s.io/pod-group-name` | label | name of the group, unique in its namespace |
| `kueue.x-k8s.io/pod-group-total-count` | annotation | number of pods in the group |

The rank comes from `batch.kubernetes.io/job-completion-index`; the head is the pod with
`ray.io/node-type: head` or the JobSet replicated job `launcher`. `/create` buffers the
members until the group is complete, answering every member before the last with no job
ID, then submits the same job as `/createPodGroup`; the pods answered without the job ID
follow the job's state at their next status. A pod that names its group without its size
is refused with 400.

A group still incomplete after `GangTimeout` (or `GANGTIMEOUT`; default `10m`) is
abandoned: the files of its buffered pods are removed. The delete of a buffered pod drops
it from its group. While `/create` buffers a group, `/createPodGroup` refuses with 503 the
pods of the same namespace sent as the same `groupUID`, and it refuses with 400 a pod
whose `kueue.x-k8s.io/pod-group-name` is not its `groupUID`.

## How the job starts: the site file

The same distributed job starts differently at every site: modules to load, `srun` or
`mpirun`, PMIx flavour, fabric libraries, NCCL and UCX settings. The site administrator
keeps that knowledge in ONE bash file, `SiteScriptPath` (or `SITESCRIPTPATH`; default
`/etc/interlink/site/site.sh`, typically a mounted ConfigMap):

- its top level runs in the batch shell at the start of every gang job;
- each `<name>-launcher` function starts a gang whose head pod carries
  `interlink.eu/launcher: <name>`.

The function is called once, from the batch script on the head's node, with the head
pod's command as arguments. It starts every rank itself (`srun`, `mpirun`) and its return
code ends every pod of the gang. Without `interlink.eu/launcher`, each pod runs its own
command on its own node (rank i on the i-th node of the allocation), with `RANK`,
`WORLD_SIZE`, `MASTER_ADDR` and `MASTER_PORT` set; the head's
`slurm-job.vk.io/pre-exec` runs in the head's rank only.

The plugin exports, for the function:

| Variable | Value |
|---|---|
| `GANG_NODES` | allocated hosts, in job order (space separated) |
| `GANG_NNODES` | number of nodes |
| `GANG_HEAD_NODE` | the head's host (the batch node) |
| `GANG_GPUS_PER_NODE` | GPUs per node, from `--gres=gpu:N` or `--gpus-per-node` |
| `MASTER_ADDR`, `MASTER_PORT` | the head's address and a coordination port |
| `GANG_CONTAINER` | the head pod's container exec prefix (runtime options and image), to use unquoted |
| `GANG_LOG_DIR` | rank i's log, shown as pod i's log, is `$GANG_LOG_DIR/rank-<i>.out`; whatever the function prints itself goes to rank 0 |
| `INTERLINK_SOCKET_DIR` | the head pod's socket directory, when the site has one |

With `srun`, `--output="$GANG_LOG_DIR/rank-%n.out" --open-mode=append` gives every pod its
own log. A comment line `# interlink-tasks-per-gpu: <name> ...` makes those launchers get
one Slurm task per GPU (for `mpirun`, which takes its slots from the allocation); the
others get one task per node.

```bash
# interlink-tasks-per-gpu: mpi

module load singularity
export SINGULARITYENV_NCCL_SOCKET_IFNAME=ib0

torchrun-launcher() {
  srun --nodes="$GANG_NNODES" --ntasks-per-node=1 \
    --output="$GANG_LOG_DIR/rank-%n.out" --open-mode=append \
    $GANG_CONTAINER torchrun --nnodes="$GANG_NNODES" --nproc-per-node="$GANG_GPUS_PER_NODE" \
      --rdzv-backend=c10d --rdzv-endpoint="$MASTER_ADDR:$MASTER_PORT" --no-python "$@"
}

mpi-launcher() {
  module load openmpi
  mpirun -n $((GANG_NNODES * GANG_GPUS_PER_NODE)) $GANG_CONTAINER "$@"
}
```

The file is read and checked with `bash -n` at every gang submission, so an update applies
to the next job; a copy goes next to the job script, so queued jobs keep the version they
were submitted with. When a pod asks for a launcher that the site file does not define,
the submission fails rather than starting the job some other way.

Probes check a pod's own command, which a launcher replaces: for a gang started by a
launcher, the containers are reported ready while the job runs.

## GPUs

`GPUFlag` (or `GPUFLAG`) sets how a pod's `nvidia.com/gpu` (or `amd.com/gpu`) request
reaches Slurm: `gres` (`--gres=gpu:N`, the default), `gpus-per-node` or `gpus`. A GPU flag
set in a flavor or in the `slurm-job.vk.io/flags` annotation takes precedence.

A group's job gets the pod's GPUs on every node. `gpus` counts the GPUs of the whole job,
so there the job asks for the pod's count times the number of nodes, as `POST /estimate`
does for the same shape.
