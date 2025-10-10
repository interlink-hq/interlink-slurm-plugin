package batchsystem

import (
	"time"
)

// CreateResult represents the response returned after successfully submitting a pod to a batch system.
// It contains the pod's unique identifier and the assigned batch job ID.
type CreateResult struct {
	PodUID string `json:"PodUID"` // Unique identifier of the Kubernetes pod
	PodJID string `json:"PodJID"` // Batch system job ID assigned to the pod
}

// LogOptions defines options for retrieving container logs from the batch system.
type LogOptions struct {
	Tail      int  // Number of lines from the end of the logs to show
	Follow    bool // Whether to stream logs continuously
	Previous  bool // Whether to retrieve logs from a previous terminated container
	Timestamps bool // Whether to include timestamps in the log output
}

// JobMetadata contains tracking information for a batch system job associated with a pod.
// This structure maintains the relationship between Kubernetes pods and batch jobs,
// tracking execution timing and job identifiers.
type JobMetadata struct {
	JID       string    // Batch system job identifier
	PodUID    string    // Kubernetes pod UID
	PodName   string    // Kubernetes pod name
	Namespace string    // Kubernetes namespace
	StartTime time.Time // When the job started execution
	EndTime   time.Time // When the job finished (zero if still running)
}

// ResourceLimits defines the computational resource constraints for a batch job.
// These are translated from Kubernetes resource requests/limits into batch system
// allocation parameters (e.g., SLURM's --cpus-per-task and --mem flags).
type ResourceLimits struct {
	CPU    int64 // CPU cores (translated to batch system CPU allocation)
	Memory int64 // Memory in bytes (translated to batch system memory allocation)
	GPU    int64 // GPU devices (if supported by batch system)
}

// Config holds the common configuration parameters that all batch systems may need.
// Specific batch system implementations can embed this struct and add their own fields.
type Config struct {
	DataRootFolder    string `yaml:"DataRootFolder"`    // Root directory for job data and scripts
	VerboseLogging    bool   `yaml:"VerboseLogging"`    // Enable debug-level logging
	ErrorsOnlyLogging bool   `yaml:"ErrorsOnlyLogging"` // Only log errors
	Namespace         string `yaml:"Namespace"`         // Default Kubernetes namespace
	ExportPodData     bool   `yaml:"ExportPodData"`     // Whether to export pod metadata to files
}
