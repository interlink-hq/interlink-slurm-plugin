package slurm

import (
	"encoding/json"
	"net/http"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"

	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// PingResponse represents the response structure for the ping endpoint
type PingResponse struct {
	Status         string `json:"status"`
	Timestamp      string `json:"timestamp"`
	SlurmConnected bool   `json:"slurm_connected"`
	SinfoOutput    string `json:"sinfo_output,omitempty"`
	Error          string `json:"error,omitempty"`
}

// PingHandler provides a health check endpoint that includes sinfo -s output
// This allows monitoring the SLURM cluster status and node availability
func (h *SidecarHandler) PingHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "Ping", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	log.G(h.Ctx).Info("Slurm Sidecar: received Ping call")

	response := PingResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Test SLURM connectivity using sinfo -s command
	sinfoOutput, err := h.getSinfoSummary()
	if err != nil {
		log.G(h.Ctx).Warning("Failed to execute sinfo command: ", err)
		response.SlurmConnected = false
		response.Error = err.Error()
		response.Status = "warning"
	} else {
		response.SlurmConnected = true
		response.SinfoOutput = sinfoOutput
		log.G(h.Ctx).Debug("sinfo -s output: ", sinfoOutput)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.G(h.Ctx).Error("Failed to marshal ping response: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"status":"error","error":"failed to marshal response"}`))
		return
	}

	w.Write(responseBytes)
	log.G(h.Ctx).Info("Ping response sent successfully")
}

// getSinfoSummary executes 'sinfo -s' command and returns the output
func (h *SidecarHandler) getSinfoSummary() (string, error) {
	cmd := []string{"-s"}
	shell := exec.ExecTask{
		Command: h.Config.Sinfopath,
		Args:    cmd,
		Shell:   true,
	}

	execReturn, err := shell.Execute()
	if err != nil {
		return "", err
	}

	return execReturn.Stdout, nil
}