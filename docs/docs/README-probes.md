# Probes and lifecycle handlers (preStop and postStart)

This document explains how probes and lifecycle handlers (preStop and postStart) are supported by the SLURM plugin.

## Configuration
Add the following fields to your SlurmConfig.yaml to enable lifecycle handler processing:

EnablePreStop: false  # default
PreStopTimeoutSeconds: 5  # per-preStop timeout in seconds

EnablePostStart: false  # default
PostStartTimeoutSeconds: 5  # per-postStart timeout in seconds

## Usage
- preStop handlers are executed only when the SLURM job receives SIGTERM.
- postStart handlers are executed synchronously after each container starts.
- Both preStop and postStart handlers are executed in the same order containers are declared in the pod spec.
- Supported handler types: exec, httpGet (TCP not supported).

## Example

### preStop Example

apiVersion: v1
kind: Pod
metadata:
  name: prestop-example
spec:
  containers:
  - name: main
    image: busybox
    lifecycle:
      preStop:
        exec:
          command:
            - "/bin/sh"
            - "-c"
            - "echo prerun; sleep 2"
    command:
      - "/bin/sh"
      - "-c"
      - "sleep 300"

When the job receives SIGTERM, preStop handlers will run first, then probe processes will be cleaned up, and finally the job will exit.

### postStart Example

apiVersion: v1
kind: Pod
metadata:
  name: poststart-example
spec:
  containers:
  - name: main
    image: busybox
    lifecycle:
      postStart:
        exec:
          command:
            - "/bin/sh"
            - "-c"
            - "echo 'Container started'; date"
    command:
      - "/bin/sh"
      - "-c"
      - "sleep 300"

When the container starts, the postStart handler will execute synchronously after the container process begins. Note that the container runs in the background and is not blocked by the postStart handler execution.
