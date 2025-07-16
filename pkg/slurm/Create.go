package slurm

import (
	"encoding/json"
	"errors"
	"io"
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
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, http.StatusGatewayTimeout, err)
		return
	}

	containers := data.Pod.Spec.InitContainers
	containers = append(containers, data.Pod.Spec.Containers...)
	metadata := data.Pod.ObjectMeta
	filesPath := h.Config.DataRootFolder + data.Pod.Namespace + "-" + string(data.Pod.UID)

	var singularity_command_pod []SingularityCommand
	var resourceLimits ResourceLimits

	isDefaultCPU := false
	isDefaultRam := false

	for i, container := range containers {
		log.G(h.Ctx).Info("- Beginning script generation for container " + container.Name)

		singularityMounts := ""
		if singMounts, ok := metadata.Annotations["slurm-job.vk.io/singularity-mounts"]; ok {
			singularityMounts = singMounts
		}

		singularityOptions := ""
		if singOpts, ok := metadata.Annotations["slurm-job.vk.io/singularity-options"]; ok {
			singularityOptions = singOpts
		}

		// See https://github.com/interTwin-eu/interlink-slurm-plugin/issues/32#issuecomment-2416031030
		// singularity run will honor the entrypoint/command (if exist) in container image, while exec will override entrypoint.
		// Thus if pod command (equivalent to container entrypoint) exist, we do exec, and other case we do run
		singularityCommand := ""
		if len(container.Command) != 0 {
			singularityCommand = "exec"
		} else {
			singularityCommand = "run"
		}

		// no-eval is important so that singularity does not evaluate env var, because the shellquote has already done the safety check.
		commstr1 := []string{h.Config.SingularityPath, singularityCommand}
		commstr1 = append(commstr1, h.Config.SingularityDefaultOptions...)
		commstr1 = append(commstr1, singularityMounts, singularityOptions)

		image := ""

		CPULimit, _ := container.Resources.Limits.Cpu().AsInt64()
		MemoryLimit, _ := container.Resources.Limits.Memory().AsInt64()

		if CPULimit == 0 {
			log.G(h.Ctx).Warning(errors.New("Max CPU resource not set for " + container.Name + ". Only 1 CPU will be used"))
			resourceLimits.CPU += 1
			isDefaultCPU = true
		} else {
			if CPULimit > resourceLimits.CPU {
				log.G(h.Ctx).Info("Setting CPU limit to " + strconv.FormatInt(CPULimit, 10))
				resourceLimits.CPU = CPULimit
			} else {
				log.G(h.Ctx).Info("Keeping CPU limit to " + strconv.FormatInt(resourceLimits.CPU, 10))
			}
			isDefaultCPU = false
		}
		if MemoryLimit == 0 {
			log.G(h.Ctx).Warning(errors.New("Max Memory resource not set for " + container.Name + ". Only 1MB will be used"))
			resourceLimits.Memory += 1024 * 1024
			isDefaultRam = true
		} else {
			resourceLimits.Memory += MemoryLimit
			if MemoryLimit > resourceLimits.Memory {
				log.G(h.Ctx).Info("Setting Memory limit to " + strconv.FormatInt(MemoryLimit, 10))
				resourceLimits.Memory = MemoryLimit
			} else {
				log.G(h.Ctx).Info("Keeping Memory limit to " + strconv.FormatInt(resourceLimits.Memory, 10))
			}
			isDefaultRam = false
		}

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

		image = container.Image
		imagePrefix := h.Config.ImagePrefix

		imagePrefixAnnotationFound := false
		if imagePrefixAnnotation, ok := metadata.Annotations["slurm-job.vk.io/image-root"]; ok {
			// This takes precedence over ImagePrefix
			imagePrefix = imagePrefixAnnotation
			imagePrefixAnnotationFound = true
		}
		log.G(h.Ctx).Info("imagePrefix from annotation? ", imagePrefixAnnotationFound, " value: ", imagePrefix)

		// If imagePrefix begins with "/", then it must be an absolute path instead of for example docker://some/image.
		// The file should be one of https://docs.sylabs.io/guides/3.1/user-guide/cli/singularity_run.html#synopsis format.
		if strings.HasPrefix(image, "/") {
			log.G(h.Ctx).Warningf("image set to %s is an absolute path. Prefix won't be added.", image)
		} else if !strings.HasPrefix(image, imagePrefix) {
			image = imagePrefix + container.Image
		} else {
			log.G(h.Ctx).Warningf("imagePrefix set to %s but already present in the image name %s. Prefix won't be added.", imagePrefix, image)
		}

		log.G(h.Ctx).Debug("-- Appending all commands together...")
		singularity_command := append(commstr1, envs...)
		singularity_command = append(singularity_command, mounts)
		singularity_command = append(singularity_command, image)

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

		singularity_command_pod = append(singularity_command_pod, SingularityCommand{singularityCommand: singularity_command, containerName: container.Name, containerArgs: container.Args, containerCommand: container.Command, isInitContainer: isInit})
	}

	span.SetAttributes(
		attribute.Int64("job.limits.cpu", resourceLimits.CPU),
		attribute.Int64("job.limits.memory", resourceLimits.Memory),
	)

	path, err := produceSLURMScript(spanCtx, h.Config, string(data.Pod.UID), filesPath, metadata, singularity_command_pod, resourceLimits, isDefaultCPU, isDefaultRam)
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
