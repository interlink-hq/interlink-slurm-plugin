package slurm

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"

	"github.com/containerd/containerd/log"
	"github.com/goccy/go-yaml"
)

var SlurmConfigInst SlurmConfig
var Clientset *kubernetes.Clientset

// ComputeNodeFileName is the file, inside a pod's job working directory, where
// the generated batch script records the compute node the job is running on.
const ComputeNodeFileName = "compute-node"

// readComputeNode returns the compute node recorded for the job in podDir, or an
// empty string if the job has not started yet. A missing file is the normal state
// for a queued job, not an error.
func readComputeNode(podDir string) string {
	raw, err := os.ReadFile(filepath.Join(podDir, ComputeNodeFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// socketDirNameLen is how many hex digits of the pod UID name its socket
// directory. They are random in a v4 UID, and short enough to leave room for
// socket names within the 107 usable bytes of a unix socket path.
const socketDirNameLen = 12

// maxSocketDirRootLen is the longest SocketDirRoot that still fits the longest
// socket path, <root>/<12 hex digits>/fwd-65535.sock, in 107 bytes.
const maxSocketDirRootLen = 79

// socketDirRootPattern keeps SocketDirRoot to characters that need no quoting in
// shell and ssh arguments, and refuses elements that start with a dot.
var socketDirRootPattern = regexp.MustCompile(`^(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$`)

// socketDirFor returns the pod's socket directory under root, named after the
// first hex digits of its UID. The path is computed, never read back from the
// job, so it can be exported before submission and removed on delete. It is
// empty when root is empty or the UID is too short to name a directory.
func socketDirFor(root, uid string) string {
	if root == "" {
		return ""
	}
	digits := strings.ReplaceAll(uid, "-", "")
	if len(digits) < socketDirNameLen {
		return ""
	}
	name := digits[:socketDirNameLen]
	if strings.Trim(name, "0123456789abcdefABCDEF") != "" {
		return ""
	}
	return filepath.Join(root, name)
}

// validateSocketDirRoot returns root cleaned, or an error when it is not an
// absolute path of plain characters short enough for unix socket paths.
func validateSocketDirRoot(root string) (string, error) {
	for _, elem := range strings.Split(root, string(filepath.Separator)) {
		if elem == ".." {
			return "", fmt.Errorf("SocketDirRoot %q must not contain \"..\"", root)
		}
	}
	clean := filepath.Clean(root)
	if !socketDirRootPattern.MatchString(clean) {
		return "", fmt.Errorf("SocketDirRoot %q must be an absolute path of letters, digits, '.', '_' and '-', with no element starting with '.'", root)
	}
	if len(clean) > maxSocketDirRootLen {
		return "", fmt.Errorf(`SocketDirRoot must be at most 79 bytes: 79 + "/" + 12 + "/fwd-65535.sock" = 107, the longest unix socket path (%q is %d)`, clean, len(clean))
	}
	return clean, nil
}

// TODO: implement factory design

// NewSlurmConfig returns a variable of type SlurmConfig, used in many other functions and the first encountered error.
func NewSlurmConfig() (SlurmConfig, error) {
	if !SlurmConfigInst.set {
		var path string
		verbose := flag.Bool("verbose", false, "Enable or disable Debug level logging")
		errorsOnly := flag.Bool("errorsonly", false, "Prints only errors if enabled")
		SlurmConfigPath := flag.String("SlurmConfigpath", "", "Path to InterLink config")
		flag.Parse()

		if *verbose {
			SlurmConfigInst.VerboseLogging = true
			SlurmConfigInst.ErrorsOnlyLogging = false
		} else if *errorsOnly {
			SlurmConfigInst.VerboseLogging = false
			SlurmConfigInst.ErrorsOnlyLogging = true
		}

		if *SlurmConfigPath != "" {
			path = *SlurmConfigPath
		} else if os.Getenv("SLURMCONFIGPATH") != "" {
			path = os.Getenv("SLURMCONFIGPATH")
		} else {
			path = "/etc/interlink/SlurmConfig.yaml"
		}

		if _, err := os.Stat(path); err != nil {
			log.G(context.Background()).Error("File " + path + " doesn't exist. You can set a custom path by exporting SLURMCONFIGPATH. Exiting...")
			return SlurmConfig{}, err
		}

		log.G(context.Background()).Info("Loading SLURM config from " + path)
		yfile, err := os.ReadFile(path)
		if err != nil {
			log.G(context.Background()).Error("Error opening config file, exiting...")
			return SlurmConfig{}, err
		}
		yaml.Unmarshal(yfile, &SlurmConfigInst)

		if os.Getenv("SIDECARPORT") != "" {
			SlurmConfigInst.Sidecarport = os.Getenv("SIDECARPORT")
		}

		if os.Getenv("SBATCHPATH") != "" {
			SlurmConfigInst.Sbatchpath = os.Getenv("SBATCHPATH")
		}

		if os.Getenv("SCANCELPATH") != "" {
			SlurmConfigInst.Scancelpath = os.Getenv("SCANCELPATH")
		}

		if os.Getenv("SINFOPATH") != "" {
			SlurmConfigInst.Sinfopath = os.Getenv("SINFOPATH")
		}

		if os.Getenv("SINGULARITYPATH") != "" {
			SlurmConfigInst.SingularityPath = os.Getenv("SINGULARITYPATH")
		}

		if os.Getenv("TSOCKS") != "" {
			if os.Getenv("TSOCKS") != "true" && os.Getenv("TSOCKS") != "false" {
				fmt.Println("export TSOCKS as true or false")
				return SlurmConfig{}, err
			}
			if os.Getenv("TSOCKS") == "true" {
				SlurmConfigInst.Tsocks = true
			} else {
				SlurmConfigInst.Tsocks = false
			}
		}

		if os.Getenv("TSOCKSPATH") != "" {
			path = os.Getenv("TSOCKSPATH")
			if _, err := os.Stat(path); err != nil {
				log.G(context.Background()).Error("File " + path + " doesn't exist. You can set a custom path by exporting TSOCKSPATH. Exiting...")
				return SlurmConfig{}, err
			}

			SlurmConfigInst.Tsockspath = path
		}

		// Set default ContainerRuntime if not configured
		if SlurmConfigInst.ContainerRuntime == "" {
			SlurmConfigInst.ContainerRuntime = "singularity"
		}

		// Check if a supported container runtime is configured (supported: singularity, enroot)
		if SlurmConfigInst.ContainerRuntime != "singularity" && SlurmConfigInst.ContainerRuntime != "enroot" {
			err := fmt.Errorf("container runtime %q is not supported. Please configure a supported one (singularity, enroot)", SlurmConfigInst.ContainerRuntime)
			return SlurmConfig{}, err
		}

		// Set default SingularityPath if not configured
		if SlurmConfigInst.SingularityPath == "" {
			SlurmConfigInst.SingularityPath = "singularity"
		}

		// Set default SinfoPath if not configured
		if SlurmConfigInst.Sinfopath == "" {
			SlurmConfigInst.Sinfopath = "/usr/bin/sinfo"
		}

		// Gang scheduling. Off by default; opt in via config or the
		// GANGSCHEDULINGENABLED env var. GangTimeout defaults to 10m when unset,
		// and an invalid value is caught early rather than at buffer time.
		if os.Getenv("GANGSCHEDULINGENABLED") != "" {
			SlurmConfigInst.GangSchedulingEnabled = os.Getenv("GANGSCHEDULINGENABLED") == "true"
		}
		if os.Getenv("GANGTIMEOUT") != "" {
			SlurmConfigInst.GangTimeout = os.Getenv("GANGTIMEOUT")
		}
		if SlurmConfigInst.GangTimeout == "" {
			SlurmConfigInst.GangTimeout = "10m"
		}
		if os.Getenv("SITESCRIPTPATH") != "" {
			SlurmConfigInst.SiteScriptPath = os.Getenv("SITESCRIPTPATH")
		}
		if os.Getenv("GPUFLAG") != "" {
			SlurmConfigInst.GPUFlag = os.Getenv("GPUFLAG")
		}
		switch SlurmConfigInst.GPUFlag {
		case "", GPUFlagGres, GPUFlagGPUsPerNode, GPUFlagGPUs:
		default:
			return SlurmConfigInst, fmt.Errorf("GPUFlag %q: want %q, %q or %q", SlurmConfigInst.GPUFlag, GPUFlagGres, GPUFlagGPUsPerNode, GPUFlagGPUs)
		}
		if SlurmConfigInst.GangSchedulingEnabled {
			// Reject exactly what the runtime (gangGuaranteeTimeout) would refuse to
			// honour: an unparseable duration OR a non-positive one (e.g. "0s").
			// Validating and honouring the same predicate keeps config-time and
			// runtime behaviour consistent rather than silently defaulting a value
			// the operator explicitly set.
			d, err := time.ParseDuration(SlurmConfigInst.GangTimeout)
			if err != nil {
				return SlurmConfig{}, fmt.Errorf("invalid GangTimeout %q: %w", SlurmConfigInst.GangTimeout, err)
			}
			if d <= 0 {
				return SlurmConfig{}, fmt.Errorf("invalid GangTimeout %q: must be a positive duration", SlurmConfigInst.GangTimeout)
			}
			log.G(context.Background()).Infof("Gang scheduling ENABLED (pod groups through /createPodGroup, and through /create with a buffer timeout of %s)", SlurmConfigInst.GangTimeout)
		}

		SlurmConfigInst.set = true

		if len(SlurmConfigInst.SingularityDefaultOptions) == 0 {
			SlurmConfigInst.SingularityDefaultOptions = []string{"--nv", "--no-eval", "--containall"}
		}

		// Validate and log flavor configuration
		if len(SlurmConfigInst.Flavors) > 0 {
			log.G(context.Background()).Infof("Loaded %d flavor(s):", len(SlurmConfigInst.Flavors))
			for name, flavor := range SlurmConfigInst.Flavors {
				if flavor.Name == "" {
					log.G(context.Background()).Warningf("Flavor '%s' has no Name field set, using key as name", name)
					flavor.Name = name
					SlurmConfigInst.Flavors[name] = flavor
				}

				// Validate the flavor configuration
				if err := flavor.Validate(); err != nil {
					log.G(context.Background()).Errorf("Invalid flavor configuration for '%s': %v", name, err)
					return SlurmConfig{}, fmt.Errorf("invalid flavor '%s': %w", name, err)
				}

				log.G(context.Background()).Infof("  - %s: %s (CPU: %d, Memory: %s, SLURM flags: %d)",
					flavor.Name, flavor.Description, flavor.CPUDefault, flavor.MemoryDefault, len(flavor.SlurmFlags))
			}

			// Validate DefaultFlavor if set
			if SlurmConfigInst.DefaultFlavor != "" {
				if _, exists := SlurmConfigInst.Flavors[SlurmConfigInst.DefaultFlavor]; !exists {
					log.G(context.Background()).Warningf("DefaultFlavor '%s' not found in Flavors map, ignoring", SlurmConfigInst.DefaultFlavor)
					SlurmConfigInst.DefaultFlavor = ""
				} else {
					log.G(context.Background()).Infof("Default flavor set to: %s", SlurmConfigInst.DefaultFlavor)
				}
			}
		} else {
			log.G(context.Background()).Info("No flavors configured, using default behavior")
		}

		// Validate and log UID configuration (RFC: https://github.com/interlink-hq/interlink-slurm-plugin/discussions/58)
		if SlurmConfigInst.DefaultUID != nil {
			if *SlurmConfigInst.DefaultUID < 0 {
				err := fmt.Errorf("DefaultUID cannot be negative (got %d)", *SlurmConfigInst.DefaultUID)
				log.G(context.Background()).Error(err)
				return SlurmConfig{}, err
			}
			log.G(context.Background()).Infof("Default UID set to: %d (jobs will run as this user unless overridden by pod securityContext)", *SlurmConfigInst.DefaultUID)
		}

		if SlurmConfigInst.SocketDirRoot != "" {
			SlurmConfigInst.SocketDirRoot, err = validateSocketDirRoot(SlurmConfigInst.SocketDirRoot)
			if err != nil {
				log.G(context.Background()).Error(err)
				return SlurmConfig{}, err
			}
			log.G(context.Background()).Infof("Per-pod socket directories under: %s", SlurmConfigInst.SocketDirRoot)
		}
	}
	return SlurmConfigInst, nil
}

func (h *SidecarHandler) handleError(ctx context.Context, w http.ResponseWriter, statusCode int, err error) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("An error occurred:" + err.Error())
	w.WriteHeader(statusCode)
	w.Write([]byte(err.Error()))
	log.G(h.Ctx).Error(err)
}

func (h *SidecarHandler) logErrorVerbose(context string, ctx context.Context, w http.ResponseWriter, err error) {
	errWithContext := fmt.Errorf("error context: %s type: %s %w", context, fmt.Sprintf("%#v", err), err)
	log.G(h.Ctx).Error(errWithContext)
	h.handleError(ctx, w, http.StatusInternalServerError, errWithContext)
}

func GetSessionContext(r *http.Request) string {
	sessionContext := r.Header.Get("InterLink-Http-Session")
	if sessionContext == "" {
		sessionContext = "NoSessionFound#0"
	}
	return sessionContext
}

func GetSessionContextMessage(sessionContext string) string {
	return "HTTP InterLink session " + sessionContext + ": "
}
