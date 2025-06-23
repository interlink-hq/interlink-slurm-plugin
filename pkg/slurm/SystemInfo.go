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

// SystemInfoResponse represents the response structure for the system-info endpoint
type SystemInfoResponse struct {
	Status         string `json:"status"`
	Timestamp      string `json:"timestamp"`
	SlurmConnected bool   `json:"slurm_connected"`
	SinfoOutput    string `json:"sinfo_output,omitempty"`
	Error          string `json:"error,omitempty"`
}

// SystemInfoHandler provides a health check endpoint that includes sinfo -s output
// This allows monitoring the SLURM cluster status and node availability
func (h *SidecarHandler) SystemInfoHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "SystemInfo", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	log.G(h.Ctx).Info("Slurm Sidecar: received SystemInfo call")

	response := SystemInfoResponse{
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
		log.G(h.Ctx).Error("Failed to marshal system info response: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"status":"error","error":"failed to marshal response"}`))
		return
	}

	w.Write(responseBytes)
	log.G(h.Ctx).Info("SystemInfo response sent successfully")
}