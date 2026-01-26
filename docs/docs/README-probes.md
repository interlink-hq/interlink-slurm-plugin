# Probes and preStop handlers

This document explains how probes and preStop lifecycle handlers are supported by the SLURM plugin.

## Configuration
Add the following fields to your SlurmConfig.yaml to enable preStop processing:

EnablePreStop: false  # default
PreStopTimeoutSeconds: 5  # per-preStop timeout in seconds

## Usage
- preStop handlers are executed only when the SLURM job receives SIGTERM.
- preStop handlers are executed synchronously in the same order containers are declared in the pod spec.
- Supported handler types: exec, httpGet (TCP not supported).

## Example

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
