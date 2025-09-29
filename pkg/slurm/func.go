package slurm

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes"

	"github.com/containerd/containerd/log"
	"gopkg.in/yaml.v2"
)

var SlurmConfigInst SlurmConfig
var Clientset *kubernetes.Clientset

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

		SlurmConfigInst.set = true

		if len(SlurmConfigInst.SingularityDefaultOptions) == 0 {
			SlurmConfigInst.SingularityDefaultOptions = []string{"--nv", "--no-eval", "--containall"}
		}
	}
	return SlurmConfigInst, nil
}

func (h *SidecarHandler) handleError(ctx context.Context, w http.ResponseWriter, statusCode int, err error) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("An error occurred:" + err.Error())
	w.WriteHeader(statusCode)
	w.Write([]byte("Some errors occurred while creating container. Check Slurm Sidecar's logs"))
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
