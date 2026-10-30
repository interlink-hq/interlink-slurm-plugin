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

	// Drop this pod from its gang entry (if any) before deleting the container.
	// interLink /delete is per-pod, so the N members of a gang arrive as N
	// separate deletes. Each is removed from the entry, which is dropped once
	// empty; the shared JID is refcounted below and scancelled only on the last
	// member. A no-op for non-gang pods.
	h.removeGangMemberOnDelete(spanCtx, string(pod.UID))
	// Resolve the job working directory.
	// Priority: 1) in-memory JIDs cache  2) pod annotation (handles wiped cache)
	workDir := getJobWorkDir(h.Config, pod.Annotations, pod.Namespace, string(pod.UID))
	// Under GangMu: the deletes of a gang's members arrive together and remove
	// their JIDs entries under it.
	h.GangMu.Lock()
	if jid, ok := (*h.JIDs)[string(pod.UID)]; ok && jid.WorkDir != "" {
		workDir = jid.WorkDir
	}
	h.GangMu.Unlock()

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

	// Split the delete so GangMu is held ONLY for the fast, must-be-consistent
	// scancel decision (refcount read + scancel + removeJID), and RELEASED before
	// the slow filesystem cleanup (RemoveAll, which can sleep 5s in follow mode).
	// Holding GangMu across that sleep would stall every concurrent gang
	// Create/Delete. Under the narrow lock exactly one delete sees the JID
	// refcount reach 1 and issues the single scancel.
	h.GangMu.Lock()
	jid, scancelErr := scancelDecideAndRemoveJID(spanCtx, h.Config, string(pod.UID), h.JIDs)
	h.GangMu.Unlock()
	if scancelErr != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, scancelErr)
		return
	}

	// Filesystem cleanup runs unlocked.
	err = removeJobFilesWithRetry(spanCtx, span, string(pod.UID), jid, containerDeletePath)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	// The job's EXIT trap removes the socket directory, unless the job is killed
	// first. Best-effort: the root may not be visible from here. The path is
	// recomputed, never read from a file the job could have written.
	if socketDir := socketDirFor(h.Config.SocketDirRoot, string(pod.UID)); socketDir != "" {
		if rmErr := os.RemoveAll(socketDir); rmErr != nil {
			log.G(h.Ctx).Warning("Unable to remove socket directory ", socketDir, ": ", rmErr)
		}
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
