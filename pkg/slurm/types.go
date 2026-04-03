package slurm

import (
	"fmt"
	"strings"
)

// FlavorConfig holds the configuration for a specific flavor
type FlavorConfig struct {
	Name          string   `yaml:"Name"`
	Description   string   `yaml:"Description"`
	CPUDefault    int64    `yaml:"CPUDefault"`
	MemoryDefault string   `yaml:"MemoryDefault"` // e.g., "16G", "32000M", "1024"
	UID           *int64   `yaml:"UID"`           // Optional User ID for this flavor
	SlurmFlags    []string `yaml:"SlurmFlags"`
}

// Validate checks if the FlavorConfig is valid
func (f *FlavorConfig) Validate() error {
	if f.Name == "" {
		return fmt.Errorf("flavor Name cannot be empty")
	}

	if f.CPUDefault < 0 {
		return fmt.Errorf("flavor '%s': CPUDefault cannot be negative (got %d)", f.Name, f.CPUDefault)
	}

	if f.MemoryDefault != "" {
		// Try to parse the memory string to ensure it's valid
		if _, err := parseMemoryString(f.MemoryDefault); err != nil {
			return fmt.Errorf("flavor '%s': invalid MemoryDefault format '%s': %w", f.Name, f.MemoryDefault, err)
		}
	}

	// Validate SLURM flags format (basic check)
	for i, flag := range f.SlurmFlags {
		flag = strings.TrimSpace(flag)
		if flag == "" {
			return fmt.Errorf("flavor '%s': SLURM flag at index %d is empty", f.Name, i)
		}
		// Check if flag starts with -- or -
		if !strings.HasPrefix(flag, "--") && !strings.HasPrefix(flag, "-") {
			return fmt.Errorf("flavor '%s': SLURM flag '%s' should start with '--' or '-'", f.Name, flag)
		}
	}

	// Validate UID if set
	if f.UID != nil && *f.UID < 0 {
		return fmt.Errorf("flavor '%s': UID cannot be negative (got %d)", f.Name, *f.UID)
	}

	return nil
}

// InterLinkConfig holds the whole configuration
type SlurmConfig struct {
	VKConfigPath              string   `yaml:"VKConfigPath"`
	Sbatchpath                string   `yaml:"SbatchPath"`
	Scancelpath               string   `yaml:"ScancelPath"`
	Squeuepath                string   `yaml:"SqueuePath"`
	Sinfopath                 string   `yaml:"SinfoPath"`
	Sidecarport               string   `yaml:"SidecarPort"`
	Socket                    string   `yaml:"Socket"`
	ExportPodData             bool     `yaml:"ExportPodData"`
	Commandprefix             string   `yaml:"CommandPrefix"`
	ImagePrefix               string   `yaml:"ImagePrefix"`
	DataRootFolder            string   `yaml:"DataRootFolder"`
	Namespace                 string   `yaml:"Namespace"`
	Tsocks                    bool     `yaml:"Tsocks"`
	Tsockspath                string   `yaml:"TsocksPath"`
	Tsockslogin               string   `yaml:"TsocksLoginNode"`
	BashPath                  string   `yaml:"BashPath"`
	VerboseLogging            bool     `yaml:"VerboseLogging"`
	ErrorsOnlyLogging         bool     `yaml:"ErrorsOnlyLogging"`
	SingularityDefaultOptions []string `yaml:"SingularityDefaultOptions"`
	SingularityPrefix         string   `yaml:"SingularityPrefix"`
	SingularityPath           string   `yaml:"SingularityPath"`
	EnableProbes              bool     `yaml:"EnableProbes"`
	set                       bool
	EnrootDefaultOptions      []string                `yaml:"EnrootDefaultOptions" default:"[\"--rw\"]"`
	EnrootPrefix              string                  `yaml:"EnrootPrefix"`
	EnrootPath                string                  `yaml:"EnrootPath"`
	ContainerRuntime          string                  `yaml:"ContainerRuntime" default:"singularity"` // "singularity" or "enroot"
	Flavors                   map[string]FlavorConfig `yaml:"Flavors"`
	DefaultFlavor             string                  `yaml:"DefaultFlavor"`
	DefaultUID                *int64                  `yaml:"DefaultUID"` // Optional default User ID for all jobs (RFC: https://github.com/interlink-hq/interlink-slurm-plugin/discussions/58)
}

type CreateStruct struct {
	PodUID string `json:"PodUID"`
	PodJID string `json:"PodJID"`
}

type ProbeType string

const (
	ProbeTypeHTTP ProbeType = "http"
	ProbeTypeExec ProbeType = "exec"
)

type ProbeCommand struct {
	Type                ProbeType
	HTTPGetAction       *HTTPGetAction
	ExecAction          *ExecAction
	InitialDelaySeconds int32
	PeriodSeconds       int32
	TimeoutSeconds      int32
	SuccessThreshold    int32
	FailureThreshold    int32
}

type HTTPGetAction struct {
	Path   string
	Port   int32
	Host   string
	Scheme string
}

type ExecAction struct {
	Command []string
}

type ContainerCommand struct {
	containerName    string
	isInitContainer  bool
	runtimeCommand   []string
	containerCommand []string
	containerArgs    []string
	containerImage   string
	readinessProbes  []ProbeCommand
	livenessProbes   []ProbeCommand
	startupProbes    []ProbeCommand
}

// PingResponse represents the optional structured response from the plugin ping path.
// Aligned with interlink-hq/interLink#516: when the interlink VK parses a successful
// ping response it tries to unmarshal the body as PingResponse; if a non-nil Resources
// field is present it calls updateNodeResources() so that the virtual node's Capacity
// and Allocatable fields are kept in sync with the SLURM cluster state.
//
// TODO: Replace these locally-defined types with the upstream commonIL.PingResponse,
// commonIL.ResourcesResponse, and commonIL.AcceleratorResponse once
// interlink-hq/interLink#516 is merged and the interlink dependency is updated.
type PingResponse struct {
	// Status is a short string indicating the plug-in health (e.g. "ok").
	Status string `json:"status,omitempty"`
	// Resources optionally contains the cluster resource availability that the VK
	// should use to update the virtual node capacity.
	Resources *ResourcesResponse `json:"resources,omitempty"`
}

// ResourcesResponse carries Kubernetes-quantity strings for each resource dimension.
// Omitted fields leave the current node capacity unchanged (partial updates are fine).
type ResourcesResponse struct {
	// CPU is the total available CPU as a Kubernetes quantity string (e.g. "128", "4000m").
	CPU string `json:"cpu,omitempty"`
	// Memory is the total available memory as a Kubernetes quantity string (e.g. "512Gi", "256000Mi").
	Memory string `json:"memory,omitempty"`
	// Pods is the maximum number of concurrent pods the cluster can accept (e.g. "1000").
	Pods string `json:"pods,omitempty"`
	// Accelerators lists hardware accelerators (GPUs, FPGAs, …) with their available counts.
	Accelerators []AcceleratorResponse `json:"accelerators,omitempty"`
}

// AcceleratorResponse represents one type of hardware accelerator reported by the plugin.
type AcceleratorResponse struct {
	// ResourceType is the Kubernetes extended-resource name (e.g. "nvidia.com/gpu").
	ResourceType string `json:"resourceType"`
	// Available is the count expressed as a Kubernetes quantity string (e.g. "8").
	Available string `json:"available"`
}

// slurmNodeList is the minimal schema needed to decode the top-level `sinfo --json`
// response.  Only the fields used by getClusterResources are listed; the rest are
// silently ignored via `json:"-"` / omission.
type slurmNodeList struct {
	Nodes []slurmJSONNode `json:"nodes"`
}

// slurmJSONNode represents one node entry from `sinfo --json`.  Field names follow
// SLURM's own JSON keys (snake_case).
type slurmJSONNode struct {
	CPUs        int64 `json:"cpus"`
	AllocCPUs   int64 `json:"alloc_cpus"`
	RealMemory  int64 `json:"real_memory"`  // MB
	FreeMemory  int64 `json:"free_memory"`  // MB
	AllocMemory int64 `json:"alloc_memory"` // MB
}
