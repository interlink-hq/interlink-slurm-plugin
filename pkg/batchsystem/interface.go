// Package batchsystem defines a scheduler-agnostic interface for running
// Kubernetes pods on HPC batch systems, together with a small factory used to
// register and instantiate concrete implementations (SLURM today; PBS, LSF,
// HTCondor, ... in the future).
package batchsystem

import (
	"context"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
)

// BatchSystem is the contract every batch scheduler backend must satisfy.
// The interLink plugin API (create / delete / status / getLogs / system-info)
// maps one-to-one onto these methods, so the HTTP layer can stay scheduler
// agnostic.
type BatchSystem interface {
	// Initialize performs any setup required before serving requests, such as
	// creating working directories or reloading persisted job state.
	Initialize(ctx context.Context) error

	// Create translates the pod into a batch job, submits it and returns the
	// pod UID together with the scheduler-assigned job ID.
	Create(ctx context.Context, pod commonIL.RetrievedPodData) (CreateResult, error)

	// Delete cancels the job backing the pod and cleans up its resources
	// (scripts, logs, exported ConfigMaps/Secrets, ...).
	Delete(ctx context.Context, pod *v1.Pod) error

	// Status queries the scheduler for the given pods and maps job states to
	// Kubernetes pod/container statuses.
	Status(ctx context.Context, pods []*v1.Pod) ([]commonIL.PodStatus, error)

	// GetLogs returns the output of a single container of a pod. Follow mode
	// is not part of this interface; implementations may ignore opts.Follow.
	GetLogs(ctx context.Context, pod *v1.Pod, containerName string, opts LogOptions) ([]byte, error)

	// GetSystemInfo returns a free-form, human-readable description of the
	// cluster (e.g. partitions and node states).
	GetSystemInfo(ctx context.Context) (string, error)
}
