package slurm

// SlurmConfig holds the complete configuration for the SLURM sidecar plugin.
// It defines paths to SLURM commands, container runtime settings, networking options,
// and various operational parameters that control how Kubernetes pods are translated
// into SLURM batch jobs.
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
	EnrootDefaultOptions      []string `yaml:"EnrootDefaultOptions" default:"[\"--rw\"]"`
	EnrootPrefix              string   `yaml:"EnrootPrefix"`
	EnrootPath                string   `yaml:"EnrootPath"`
	ContainerRuntime          string   `yaml:"ContainerRuntime" default:"singularity"` // "singularity" or "enroot"
}

// CreateStruct represents the response returned after successfully submitting a pod to SLURM.
// It contains the pod's unique identifier and the assigned SLURM job ID.
type CreateStruct struct {
	PodUID string `json:"PodUID"` // Unique identifier of the Kubernetes pod
	PodJID string `json:"PodJID"` // SLURM job ID assigned to the pod
}

// ProbeType defines the type of health check probe to execute.
type ProbeType string

const (
	ProbeTypeHTTP ProbeType = "http" // HTTP GET request probe
	ProbeTypeExec ProbeType = "exec" // Command execution probe
)

// ProbeCommand represents a container health check probe configuration,
// translated from Kubernetes probe specifications (readiness, liveness, or startup probes).
type ProbeCommand struct {
	Type                ProbeType      // Type of probe (HTTP or Exec)
	HTTPGetAction       *HTTPGetAction // Configuration for HTTP probes
	ExecAction          *ExecAction    // Configuration for exec probes
	InitialDelaySeconds int32          // Delay before first probe execution
	PeriodSeconds       int32          // Interval between probe executions
	TimeoutSeconds      int32          // Timeout for probe execution
	SuccessThreshold    int32          // Consecutive successes required for success
	FailureThreshold    int32          // Consecutive failures required for failure
}

// HTTPGetAction defines the parameters for an HTTP GET probe.
type HTTPGetAction struct {
	Path   string // HTTP path to request
	Port   int32  // Port to connect to
	Host   string // Host to connect to (defaults to pod IP)
	Scheme string // HTTP or HTTPS
}

// ExecAction defines a command-based probe that executes in the container.
type ExecAction struct {
	Command []string // Command and arguments to execute
}

// ContainerCommand encapsulates all information needed to execute a container
// within a SLURM job, including runtime configuration, probes, and execution context.
type ContainerCommand struct {
	containerName    string         // Name of the container
	isInitContainer  bool           // Whether this is an init container
	runtimeCommand   []string       // Container runtime command (singularity/enroot with flags)
	containerCommand []string       // Override for container entrypoint
	containerArgs    []string       // Arguments to pass to the container
	containerImage   string         // Container image reference
	readinessProbes  []ProbeCommand // Readiness probes to execute
	livenessProbes   []ProbeCommand // Liveness probes to execute
	startupProbes    []ProbeCommand // Startup probes to execute
}
