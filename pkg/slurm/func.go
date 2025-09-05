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

var ConfigInst Config
var Clientset *kubernetes.Clientset

// TODO: implement factory design

// NewConfig returns a variable of type Config, used in many other functions and the first encountered error.
func NewConfig() (Config, error) {
	if !ConfigInst.set {
		var path string
		verbose := flag.Bool("verbose", false, "Enable or disable Debug level logging")
		errorsOnly := flag.Bool("errorsonly", false, "Prints only errors if enabled")
		ConfigPath := flag.String("Configpath", "", "Path to InterLink config")
		flag.Parse()

		if *verbose {
			ConfigInst.VerboseLogging = true
			ConfigInst.ErrorsOnlyLogging = false
		} else if *errorsOnly {
			ConfigInst.VerboseLogging = false
			ConfigInst.ErrorsOnlyLogging = true
		}

		switch {
		case *ConfigPath != "":
			path = *ConfigPath
		case os.Getenv("SLURMCONFIGPATH") != "":
			path = os.Getenv("SLURMCONFIGPATH")
		default:
			path = "/etc/interlink/Config.yaml"
		}

		if _, err := os.Stat(path); err != nil {
			log.G(context.Background()).Error("File " + path + " doesn't exist. You can set a custom path by exporting SLURMCONFIGPATH. Exiting...")
			return Config{}, err
		}

		log.G(context.Background()).Info("Loading SLURM config from " + path)
		yfile, err := os.ReadFile(path)
		if err != nil {
			log.G(context.Background()).Error("Error opening config file, exiting...")
			return Config{}, err
		}
		if err := yaml.Unmarshal(yfile, &ConfigInst); err != nil {
			log.G(context.Background()).Error("Error unmarshaling config file: ", err)
			return Config{}, err
		}

		if os.Getenv("SIDECARPORT") != "" {
			ConfigInst.Sidecarport = os.Getenv("SIDECARPORT")
		}

		if os.Getenv("SBATCHPATH") != "" {
			ConfigInst.Sbatchpath = os.Getenv("SBATCHPATH")
		}

		if os.Getenv("SCANCELPATH") != "" {
			ConfigInst.Scancelpath = os.Getenv("SCANCELPATH")
		}

		if os.Getenv("SINFOPATH") != "" {
			ConfigInst.Sinfopath = os.Getenv("SINFOPATH")
		}

		if os.Getenv("SINGULARITYPATH") != "" {
			ConfigInst.SingularityPath = os.Getenv("SINGULARITYPATH")
		}

		if os.Getenv("TSOCKS") != "" {
			if os.Getenv("TSOCKS") != tsocksTrue && os.Getenv("TSOCKS") != tsocksFalse {
				fmt.Println("export TSOCKS as true or false")
				return Config{}, err
			}
			if os.Getenv("TSOCKS") == tsocksTrue {
				ConfigInst.Tsocks = true
			} else {
				ConfigInst.Tsocks = false
			}
		}

		if os.Getenv("TSOCKSPATH") != "" {
			path = os.Getenv("TSOCKSPATH")
			if _, err := os.Stat(path); err != nil {
				log.G(context.Background()).Error("File " + path + " doesn't exist. You can set a custom path by exporting TSOCKSPATH. Exiting...")
				return Config{}, err
			}

			ConfigInst.Tsockspath = path
		}

		// Set default SingularityPath if not configured
		if ConfigInst.SingularityPath == "" {
			ConfigInst.SingularityPath = "singularity"
		}

		// Set default SinfoPath if not configured
		if ConfigInst.Sinfopath == "" {
			ConfigInst.Sinfopath = "/usr/bin/sinfo"
		}

		ConfigInst.set = true

		if len(ConfigInst.SingularityDefaultOptions) == 0 {
			ConfigInst.SingularityDefaultOptions = []string{"--nv", "--no-eval", "--containall"}
		}
	}
	return ConfigInst, nil
}

func (h *SidecarHandler) handleError(ctx context.Context, w http.ResponseWriter, statusCode int, err error) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("An error occurred:" + err.Error())
	w.WriteHeader(statusCode)
	if _, err := w.Write([]byte("Some errors occurred while creating container. Check Slurm Sidecar's logs")); err != nil {
		log.G(ctx).Error("Failed to write error response: ", err)
	}
	log.G(h.Ctx).Error(err)
}

func (h *SidecarHandler) logErrorVerbose(ctx context.Context, context string, w http.ResponseWriter, err error) {
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
