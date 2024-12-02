package slurm

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/containerd/containerd/log"
	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// StopHandler runs a scancel command, updating JIDs and cached statuses
func (h *SidecarHandler) StopHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "DeleteSLURM", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	// For debugging purpose, when we have many kubectl logs, we can differentiate each one.
	sessionContext := GetSessionContext(r)
	sessionContextMessage := GetSessionContextMessage(sessionContext)

	log.G(h.Ctx).Info(sessionContextMessage, "Slurm Sidecar: received Stop call")
	statusCode := http.StatusOK

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	var pod *v1.Pod
	err = json.Unmarshal(bodyBytes, &pod)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	filesPath := h.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID)

	err = deleteContainer(spanCtx, h.Config, string(pod.UID), h.JIDs, filesPath)

	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}
	if os.Getenv("SHARED_FS") != "true" {
		err = os.RemoveAll(filesPath)
		if err != nil {
			statusCode = http.StatusInternalServerError
			h.handleError(spanCtx, w, statusCode, err)
			return
		}
	}

	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))

	w.WriteHeader(statusCode)
	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred deleting containers. Check Slurm Sidecar's logs"))
	} else {
		w.Write([]byte("All containers for submitted Pods have been deleted"))
	}
}
