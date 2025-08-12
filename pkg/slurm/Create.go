package slurm

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/log"

	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"

	"regexp"
)

func parseMem(val string) (int64, error) {
	re := regexp.MustCompile(`^(\d+)([KMG]?)$`)
	m := re.FindStringSubmatch(val)
	if len(m) != 3 {
		return 0, errors.New("invalid memory format: " + val)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	switch m[2] {
	case "G":
		return n * 1024 * 1024 * 1024, nil
	case "M":
		return n * 1024 * 1024, nil
	case "K":
		return n * 1024, nil
	default:
		return n, nil
	}
}

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
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
		return
	}

	containers := data.Pod.Spec.InitContainers
	containers = append(containers, data.Pod.Spec.Containers...)
	metadata := data.Pod.ObjectMeta
	filesPath := h.Config.DataRootFolder + data.Pod.Namespace + "-" + string(data.Pod.UID)

	var runtime_command_pod []ContainerCommand
	var resourceLimits ResourceLimits

	isDefaultCPU := true
	isDefaultRam := true

	maxCPULimit := 0
	maxMemoryLimit := 0

	cpuLimit := int64(0)
	memoryLimit := int64(0)

	for i, container := range containers {
		log.G(h.Ctx).Info("- Beginning script generation for container " + container.Name)

		image := ""

		cpuLimitFloat := container.Resources.Limits.Cpu().AsApproximateFloat64()
		memoryLimitFromContainer, _ := container.Resources.Limits.Memory().AsInt64()

		cpuLimitFromContainer := int64(math.Ceil(cpuLimitFloat))

		if cpuLimitFromContainer == 0 && isDefaultCPU {
			log.G(h.Ctx).Warning(errors.New("Max CPU resource not set for " + container.Name + ". Only 1 CPU will be used"))
			resourceLimits.CPU = 1
		} else {
			if cpuLimitFromContainer > resourceLimits.CPU && maxCPULimit < int(cpuLimitFromContainer) {
				log.G(h.Ctx).Info("Setting CPU limit to " + strconv.FormatInt(cpuLimitFromContainer, 10))
				cpuLimit = cpuLimitFromContainer
				maxCPULimit = int(cpuLimitFromContainer)
				isDefaultCPU = false
			}
		}

		if memoryLimitFromContainer == 0 && isDefaultRam {
			log.G(h.Ctx).Warning(errors.New("Max Memory resource not set for " + container.Name + ". Only 1MB will be used"))
			resourceLimits.Memory = 1024 * 1024
		} else {
			if memoryLimitFromContainer > resourceLimits.Memory && maxMemoryLimit < int(memoryLimitFromContainer) {
				log.G(h.Ctx).Info("Setting Memory limit to " + strconv.FormatInt(memoryLimitFromContainer, 10))
				memoryLimit = memoryLimitFromContainer
				maxMemoryLimit = int(memoryLimitFromContainer)
				isDefaultRam = false
			}
		}

		resourceLimits.CPU = cpuLimit
		resourceLimits.Memory = memoryLimit

		mounts, err := prepareMounts(spanCtx, h.Config, &data, &container, filesPath)
		log.G(h.Ctx).Debug(mounts)
		if err != nil {
			statusCode = http.StatusInternalServerError
			h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
			os.RemoveAll(filesPath)
			return
		}

		// prepareEnvs creates a file in the working directory, that must exist. This is created at prepareMounts.
		envs := prepareEnvs(spanCtx, h.Config, data, container)
		image = prepareImage(spanCtx, h.Config, metadata, container.Image)
		commstr1 := prepareRuntimeCommand(h.Config, container, metadata)
		log.G(h.Ctx).Debug("-- Appending all commands together...")
		runtime_command := append(commstr1, envs...)
		switch h.Config.ContainerRuntime {
		case "singularity":
			runtime_command = append(runtime_command, mounts)
			runtime_command = append(runtime_command, image)
		case "enroot":
			containerName := container.Name + string(data.Pod.UID)
			mounts = strings.ReplaceAll(mounts, ":ro", "")
			runtime_command = append(runtime_command, mounts)
			runtime_command = append(runtime_command, containerName)

		}

		isInit := false

		if i < len(data.Pod.Spec.InitContainers) {
			isInit = true
		}

		span.SetAttributes(
			attribute.String("job.container"+strconv.Itoa(i)+".name", container.Name),
			attribute.Bool("job.container"+strconv.Itoa(i)+".isinit", isInit),
			attribute.StringSlice("job.container"+strconv.Itoa(i)+".envs", envs),
			attribute.String("job.container"+strconv.Itoa(i)+".image", image),
			attribute.StringSlice("job.container"+strconv.Itoa(i)+".command", container.Command),
			attribute.StringSlice("job.container"+strconv.Itoa(i)+".args", container.Args),
		)

		runtime_command_pod = append(runtime_command_pod, ContainerCommand{runtimeCommand: runtime_command, containerName: container.Name, containerArgs: container.Args, containerCommand: container.Command, isInitContainer: isInit, containerImage: image})
	}

	span.SetAttributes(
		attribute.Int64("job.limits.cpu", resourceLimits.CPU),
		attribute.Int64("job.limits.memory", resourceLimits.Memory),
	)

	path, err := produceSLURMScript(spanCtx, h.Config, string(data.Pod.UID), filesPath, metadata, runtime_command_pod, resourceLimits, isDefaultCPU, isDefaultRam)
	if err != nil {
		log.G(h.Ctx).Error(err)
		os.RemoveAll(filesPath)
		return
	}
	out, err := SLURMBatchSubmit(h.Ctx, h.Config, path)
	if err != nil {
		span.AddEvent("Failed to submit the SLURM Job")
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
		os.RemoveAll(filesPath)
		return
	}
	log.G(h.Ctx).Info(out)
	jid, err := handleJidAndPodUid(h.Ctx, data.Pod, h.JIDs, out, filesPath)
	if err != nil {
		statusCode = http.StatusInternalServerError
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
		w.Write([]byte("Some errors occurred while creating containers. Check Slurm Sidecar's logs"))
	} else {
		w.Write(returnedJIDBytes)
	}
}
