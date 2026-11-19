---
sidebar_position: 2
---

# Current limitations

It's not black magic, we have to pay something:

- __InCluster network__: at the moment there is no support for in-cluster communication b/w remote container and k8s cluster network. Meaning that service that are NOT exposed to the external network cannot be accessed from the pod running on the virtual kubelet. There are plans to include the support for this, we are not simply there yet.
- __Cluster wide shared FS__: there is no support for cluster-wide filesystem mounting on the remote container. The only volumes supported are: `Secret`, `ConfigMap`, `EmptyDir`
- __enroot environment__: with `ContainerRuntime: enroot` the plugin passes the pod's env through its own enroot `--conf` script, and nothing of the host's environment. enroot's environment holds one variable per line, so a pod variable whose value contains a newline (a PEM key, multi-line JSON) or whose name is not a shell identifier (`a.b`) is skipped, with only a warning in the plugin's log: mount such values as files instead. enroot itself drops names starting with `ENROOT_`, which it reserves. enroot runs only the last `--conf` it is given, and for a container with env that is the plugin's: a `-c` or `--conf` in `EnrootDefaultOptions` or in the `slurm-job.vk.io/enroot-options` annotation does not run for such a container, and the plugin logs a warning.

That's all. If you find anything else, feel free to let it know filing a github issue.

