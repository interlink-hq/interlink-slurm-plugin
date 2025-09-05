package slurm

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/containerd/containerd/log"

	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// SubmitHandler generates and submits a SLURM batch script according to provided data.
// 1 Pod = 1 Job. If a Pod has multiple containers, every container is a line with it's parameters in the SLURM script.
func (h *SidecarHandler) SubmitHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "Create", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	log.G(h.Ctx).Info("Slurm Sidecar: received Submit call")
	statusCode := http.StatusOK
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	var data commonIL.RetrievedPodData

	// to be changed to commonIL.CreateStruct
	var returnedJID CreateStruct // returnValue
	var returnedJIDBytes []byte
	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
		return
	}

	containers := data.Pod.Spec.InitContainers
	containers = append(containers, data.Pod.Spec.Containers...)
	metadata := data.Pod.ObjectMeta
	filesPath := h.Config.DataRootFolder + data.Pod.Namespace + "-" + string(data.Pod.UID)

	var singularityCommandPod []SingularityCommand
	var resourceLimits ResourceLimits

	isDefaultCPU := true
	isDefaultRAM := true

	maxCPULimit := 0
	maxMemoryLimit := 0

	cpuLimit := int64(0)
	memoryLimit := int64(0)

	for i, container := range containers {
		singularityCommand, err := h.processSingleContainer(
			spanCtx, span, container, i, metadata, data, filesPath,
			&resourceLimits, &maxCPULimit, &maxMemoryLimit,
			&cpuLimit, &memoryLimit, &isDefaultCPU, &isDefaultRAM,
		)
		if err != nil {
			h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
			return
		}

		singularityCommandPod = append(singularityCommandPod, *singularityCommand)
	}

	span.SetAttributes(
		attribute.Int64("job.limits.cpu", resourceLimits.CPU),
		attribute.Int64("job.limits.memory", resourceLimits.Memory),
	)

	path, err := produceSLURMScript(spanCtx, h.Config, string(data.Pod.UID), filesPath, metadata, singularityCommandPod, resourceLimits, isDefaultCPU, isDefaultRAM)
	if err != nil {
		log.G(h.Ctx).Error(err)
		os.RemoveAll(filesPath)
		return
	}
	out, err := BatchSubmit(h.Ctx, h.Config, path)
	if err != nil {
		span.AddEvent("Failed to submit the SLURM Job")
		h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
		os.RemoveAll(filesPath)
		return
	}
	log.G(h.Ctx).Info(out)
	jid, err := handleJidAndPodUID(h.Ctx, data.Pod, h.JIDs, out, filesPath)
	if err != nil {
		h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
		os.RemoveAll(filesPath)
		err = deleteContainer(spanCtx, h.Config, string(data.Pod.UID), h.JIDs, filesPath)
		if err != nil {
			log.G(h.Ctx).Error(err)
		}
		return
	}

	span.AddEvent("SLURM Job successfully submitted with ID " + jid)
	returnedJID = CreateStruct{PodUID: string(data.Pod.UID), PodJID: jid}

	returnedJIDBytes, err = json.Marshal(returnedJID)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	w.WriteHeader(statusCode)

	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))

	if statusCode != http.StatusOK {
		if _, err := w.Write([]byte("Some errors occurred while creating containers. Check Slurm Sidecar's logs")); err != nil {
			log.G(h.Ctx).Error("Failed to write error response: ", err)
		}
	} else {
		if _, err := w.Write(returnedJIDBytes); err != nil {
			log.G(h.Ctx).Error("Failed to write response: ", err)
		}
	}
}
