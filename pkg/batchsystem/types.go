package batchsystem

// CreateResult is returned by BatchSystem.Create after a successful submission.
// The JSON tags match the body the interLink API server expects from the
// plugin /create endpoint.
type CreateResult struct {
	PodUID string `json:"PodUID"` // Kubernetes pod UID
	PodJID string `json:"PodJID"` // Scheduler-assigned job ID
}

// LogOptions selects which part of a container's output to return. A zero
// value returns the full output.
type LogOptions struct {
	Tail       int  // Return only the last N lines (0 = no limit). Takes precedence over LimitBytes.
	LimitBytes int  // Return only the last N bytes (0 = no limit).
	Follow     bool // Stream logs until the container terminates (implementation-defined).
	Previous   bool // Return logs of the previous instance of the container (implementation-defined).
	Timestamps bool // Prefix lines with timestamps (implementation-defined).
}
