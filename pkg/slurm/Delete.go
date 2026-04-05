package slurm

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/containerd/containerd/log"
	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// StopHandler runs a scancel command, updating JIDs and cached statuses
func (h *SidecarHandler) StopHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "Delete", trace.WithAttributes(
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

	// Resolve the job working directory.
	// Priority: 1) in-memory JIDs cache  2) pod annotation (handles wiped cache)
	workDir := getJobWorkDir(h.Config, pod.Annotations, pod.Namespace, string(pod.UID))
	if jid, ok := (*h.JIDs)[string(pod.UID)]; ok && jid.WorkDir != "" {
		workDir = jid.WorkDir
	}

	// When the annotation "slurm-job.vk.io/no-clean-workdir" is "true" and a
	// custom workdir is in use, preserve the job output directory after deletion.
	noCleanWorkDir := workDir != filesPath && pod.Annotations["slurm-job.vk.io/no-clean-workdir"] == "true"

	// Pass the appropriate path to deleteContainer so it can cancel the SLURM job
	// and clean up the job directory.  When no-clean-workdir is set, only the
	// metadata directory (filesPath) is removed; the job workdir is kept.
	containerDeletePath := workDir
	if noCleanWorkDir {
		containerDeletePath = filesPath
	}

	err = deleteContainer(spanCtx, h.Config, string(pod.UID), h.JIDs, containerDeletePath)

	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	if os.Getenv("SHARED_FS") != "true" && !noCleanWorkDir {
		// deleteContainer already removed containerDeletePath (= workDir).
		// When a separate metadata directory exists, remove it now.
		if workDir != filesPath {
			if err = os.RemoveAll(filesPath); err != nil {
				statusCode = http.StatusInternalServerError
				h.handleError(spanCtx, w, statusCode, err)
				return
			}
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
