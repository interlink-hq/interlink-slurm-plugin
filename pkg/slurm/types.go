package slurm

// FlavorConfig holds the configuration for a specific flavor
type FlavorConfig struct {
	Name          string   `yaml:"Name"`
	Description   string   `yaml:"Description"`
	CPUDefault    int64    `yaml:"CPUDefault"`
	MemoryDefault string   `yaml:"MemoryDefault"` // e.g., "16G", "32000M", "1024"
	SlurmFlags    []string `yaml:"SlurmFlags"`
}

// InterLinkConfig holds the whole configuration
type SlurmConfig struct {
	VKConfigPath              string                  `yaml:"VKConfigPath"`
	Sbatchpath                string                  `yaml:"SbatchPath"`
	Scancelpath               string                  `yaml:"ScancelPath"`
	Squeuepath                string                  `yaml:"SqueuePath"`
	Sinfopath                 string                  `yaml:"SinfoPath"`
	Sidecarport               string                  `yaml:"SidecarPort"`
	Socket                    string                  `yaml:"Socket"`
	ExportPodData             bool                    `yaml:"ExportPodData"`
	Commandprefix             string                  `yaml:"CommandPrefix"`
	ImagePrefix               string                  `yaml:"ImagePrefix"`
	DataRootFolder            string                  `yaml:"DataRootFolder"`
	Namespace                 string                  `yaml:"Namespace"`
	Tsocks                    bool                    `yaml:"Tsocks"`
	Tsockspath                string                  `yaml:"TsocksPath"`
	Tsockslogin               string                  `yaml:"TsocksLoginNode"`
	BashPath                  string                  `yaml:"BashPath"`
	VerboseLogging            bool                    `yaml:"VerboseLogging"`
	ErrorsOnlyLogging         bool                    `yaml:"ErrorsOnlyLogging"`
	SingularityDefaultOptions []string                `yaml:"SingularityDefaultOptions"`
	SingularityPrefix         string                  `yaml:"SingularityPrefix"`
	SingularityPath           string                  `yaml:"SingularityPath"`
	EnableProbes              bool                    `yaml:"EnableProbes"`
	set                       bool
	EnrootDefaultOptions      []string                `yaml:"EnrootDefaultOptions" default:"[\"--rw\"]"`
	EnrootPrefix              string                  `yaml:"EnrootPrefix"`
	EnrootPath                string                  `yaml:"EnrootPath"`
	ContainerRuntime          string                  `yaml:"ContainerRuntime" default:"singularity"` // "singularity" or "enroot"
	Flavors                   map[string]FlavorConfig `yaml:"Flavors"`
	DefaultFlavor             string                  `yaml:"DefaultFlavor"`
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
