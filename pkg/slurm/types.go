package slurm

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
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
	// Taints is an optional list of Kubernetes taints that this plugin will advertise
	// on the virtual node via the ping response.  The VK (interLink#516) replaces the
	// node's non-system taints with this list on every heartbeat.
	Taints []TaintConfig `yaml:"Taints"`
	// ResourceScriptPath is an optional path to a script or executable that reports
	// cluster resource availability.  When set, the script is executed instead of the
	// built-in sinfo-based resource gathering.  The script must write a JSON object
	// conforming to the PingResponse schema on stdout (only the "resources" field is
	// required; "taints" and "status" are optional and, if present, are used as-is
	// unless overridden by the static Taints list above).  A non-zero exit code or
	// unparseable output is treated as a transient error and logged.
	//
	// Example script output:
	//   {"status":"ok","resources":{"cpu":"128","memory":"512Gi","pods":"1000"}}
	ResourceScriptPath string `yaml:"ResourceScriptPath"`
	// SocketDirRoot is an optional absolute directory, on a filesystem the job and
	// ssh sessions on its compute node both see, under which every job creates a
	// private 0700 directory for the pod's unix sockets. The job exports it as
	// INTERLINK_SOCKET_DIR and the status reports it to interLink, which forwards
	// the shadow's tunnels there. Put it where the plugin also sees it (inside the
	// filesystem shared for DataRootFolder) so that deleting the pod removes the
	// directory even when the job was killed before its exit trap ran. Empty
	// disables the feature. Not for deployments
	// where jobs run under more than one UID (DefaultUID, flavor UID or
	// runAsUser): a job whose account cannot write the root fails at start.
	SocketDirRoot string `yaml:"SocketDirRoot"`

	// Gang scheduling. When GangSchedulingEnabled is false (the default)
	// /createPodGroup refuses every group and the plugin behaves exactly as
	// before: 1 Pod = 1 sbatch. When enabled, the pods of a group sent in one
	// /createPodGroup call are submitted together as a single co-scheduled
	// `sbatch --nodes=N` job. /create is unchanged either way.
	GangSchedulingEnabled bool `yaml:"GangSchedulingEnabled"` // default false
	// SiteScriptPath is the site file (typically a mounted ConfigMap): its top level
	// runs at the start of every gang job and its `<name>-launcher` functions start
	// gangs whose head pod carries interlink.eu/launcher: <name>. Read at each gang
	// submission, so an edit applies to the next job. Empty => /etc/interlink/site/site.sh.
	SiteScriptPath string `yaml:"SiteScriptPath"`
	// GPUFlag is how this site's Slurm takes the GPUs a pod requests
	// (nvidia.com/gpu, amd.com/gpu): "gres" (--gres=gpu:N, the default),
	// "gpus-per-node" (--gpus-per-node=N) or "gpus" (--gpus=N).
	GPUFlag string `yaml:"GPUFlag"`
	// SiteTimeZone is the IANA time zone Slurm prints its times in (e.g.
	// "Europe/Paris"), used to read `sbatch --test-only` estimates. Empty => the
	// plugin's own time zone.
	SiteTimeZone string `yaml:"SiteTimeZone"`
}

type CreateStruct struct {
	PodUID string `json:"PodUID"`
	PodJID string `json:"PodJID"`
}

// Gang keys.
const (
	// GangRoleHead / GangRoleWorker are the two roles of a gang member; the
	// head is rank 0.
	GangRoleHead   = "head"
	GangRoleWorker = "worker"

	// LauncherAnnotation names the site launcher that starts the gang: the
	// `<name>-launcher` function of the site file (SlurmConfig.SiteScriptPath).
	// Without it, each pod runs its own command on its own node.
	LauncherAnnotation = "interlink.eu/launcher"
)

// BufferedMember holds everything produceGangSLURMScript needs to render one
// gang member's own rank line (its already-rendered singularity/enroot runtime
// command, its per-pod job dir for isolated logs, its role/rank, and the pod
// itself for command/args). One BufferedMember == one rank on one node.
type BufferedMember struct {
	PodUID          string
	Namespace       string
	FilesPath       string // per-pod dir (DataRootFolder + ns + "-" + uid): JobID.jid and the other bookkeeping files
	WorkDir         string // the pod's job dir, as /create's: job.sh, logs, status files; FilesPath unless slurm-job.vk.io/job-workdir is set
	Role            string // GangRoleHead or GangRoleWorker
	Rank            int    // 0..N-1; head is always 0
	Pod             v1.Pod
	RuntimeCommands []ContainerCommand // this member's own rendered container commands
	ResourceLimits  ResourceLimits
	IsDefaultCPU    bool
	IsDefaultRAM    bool
	Flavor          *FlavorResolution
}

// GangEntry is one gang (keyed by its name in GangTable): its members, and the
// ONE sbatch submitted for all of them.
type GangEntry struct {
	Name      string
	Size      int
	Members   map[string]*BufferedMember // keyed by PodUID
	JID       string                     // the shared SLURM job ID once submitted
	Submitted bool
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

// LifecycleHookType indicates whether a lifecycle hook is an exec or httpGet hook.
type LifecycleHookType string

const (
	LifecycleHookTypeExec    LifecycleHookType = "exec"
	LifecycleHookTypeHTTPGet LifecycleHookType = "httpGet"
)

// LifecycleHTTPGetSpec holds the parameters for an httpGet-type lifecycle hook.
type LifecycleHTTPGetSpec struct {
	Scheme string
	Host   string
	Port   int32
	Path   string
}

// LifecycleHookSpec describes a container lifecycle hook (postStart or preStop)
// in a runtime-agnostic form.
type LifecycleHookSpec struct {
	Type        LifecycleHookType
	ExecCommand []string              // populated when Type == LifecycleHookTypeExec
	HTTPGet     *LifecycleHTTPGetSpec // populated when Type == LifecycleHookTypeHTTPGet
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
	preStopHook      *LifecycleHookSpec // optional preStop lifecycle hook
	postStartHook    *LifecycleHookSpec // optional postStart lifecycle hook
}

// TaintConfig holds the configuration for a Kubernetes taint to be applied to the
// virtual node.  It is used both in SlurmConfig (loaded from YAML) and as the JSON
// payload carried in PingResponse, so it carries both yaml and json struct tags.
type TaintConfig struct {
	// Key is the taint key (e.g. "vendor.io/maintenance").
	Key string `yaml:"Key" json:"key"`
	// Value is the optional taint value.
	Value string `yaml:"Value,omitempty" json:"value,omitempty"`
	// Effect is one of "NoSchedule", "PreferNoSchedule", or "NoExecute".
	Effect string `yaml:"Effect" json:"effect"`
}

// PingResponse represents the optional structured response from the plugin ping path.
// Aligned with interlink-hq/interLink#516: when the interlink VK parses a successful
// ping response it tries to unmarshal the body as PingResponse; if a non-nil Resources
// field is present it calls updateNodeResources() and if a non-nil Taints field is
// present it calls updateNodeTaints() so that the virtual node stays in sync with the
// SLURM cluster state.
//
// TODO: Replace these locally-defined types with the upstream commonIL.PingResponse,
// commonIL.ResourcesResponse, commonIL.AcceleratorResponse, and commonIL.TaintResponse
// once interlink-hq/interLink#516 is merged and the interlink dependency is updated.
type PingResponse struct {
	// Status is a short string indicating the plug-in health (e.g. "ok").
	Status string `json:"status,omitempty"`
	// Resources optionally contains the cluster resource availability that the VK
	// should use to update the virtual node capacity.
	Resources *ResourcesResponse `json:"resources,omitempty"`
	// Taints optionally contains the list of taints the VK should apply to the virtual
	// node.  When present (even as an empty slice), all non-system taints on the node
	// are replaced with this list.  When absent, existing taints are left unchanged.
	Taints *[]TaintConfig `json:"taints,omitempty"`
	// Capabilities lists what the plugin offers beyond one job per pod, such as
	// "pod-group-create". interLink's PingResponse has no such field yet; a decoder
	// that does not know it ignores it.
	Capabilities []string `json:"capabilities,omitempty"`
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

// SlurmConfig.GPUFlag values.
const (
	GPUFlagGres        = "gres"
	GPUFlagGPUsPerNode = "gpus-per-node"
	GPUFlagGPUs        = "gpus"
)
