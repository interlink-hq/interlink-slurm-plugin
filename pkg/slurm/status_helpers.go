package slurm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// handleEmptyPodList handles the case when no pods are requested and returns sinfo output
func (h *SidecarHandler) handleEmptyPodList(spanCtx context.Context, w http.ResponseWriter) error {
	sinfoOutput, err := h.getSinfoSummary()
	if err != nil {
		log.G(h.Ctx).Warning("Failed to execute sinfo command: ", err)
		h.handleError(spanCtx, w, http.StatusInternalServerError, err)
		return err
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(sinfoOutput)); err != nil {
		log.G(h.Ctx).Error("Failed to write sinfo output: ", err)
		return err
	}
	log.G(h.Ctx).Info("Returned sinfo -s output for empty pod list")
	return nil
}

// writeTimeFile creates and writes a time file for job state tracking
func (h *SidecarHandler) writeTimeFile(spanCtx context.Context, path, filename string, timestamp time.Time, w http.ResponseWriter) error {
	f, err := os.Create(path + "/" + filename)
	if err != nil {
		h.handleError(spanCtx, w, http.StatusInternalServerError, err)
		return err
	}
	defer f.Close()

	if _, err := f.WriteString(timestamp.Format("2006-01-02 15:04:05.999999999 -0700 MST")); err != nil {
		log.G(h.Ctx).Error("Failed to write time to "+filename+": ", err)
		return err
	}
	return nil
}

// createRunningContainerStatuses creates container statuses for running jobs
func (h *SidecarHandler) createRunningContainerStatuses(spanCtx context.Context, pod *v1.Pod, path string, jidData *JidStruct) []v1.ContainerStatus {
	var containerStatuses []v1.ContainerStatus

	for _, ct := range pod.Spec.Containers {
		// Check probe status for container readiness
		readinessCount, _, err := loadProbeMetadata(path, ct.Name)
		isReady := true
		if err != nil {
			log.G(h.Ctx).Debug("Failed to load probe metadata for container ", ct.Name, ": ", err)
		} else {
			isReady = checkContainerReadiness(spanCtx, h.Config, path, ct.Name, readinessCount)
		}

		containerStatus := v1.ContainerStatus{
			Name:  ct.Name,
			State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.Time{Time: jidData.StartTime}}},
			Ready: isReady,
		}
		containerStatuses = append(containerStatuses, containerStatus)
	}

	return containerStatuses
}

// createTerminatedContainerStatuses creates container statuses for terminated jobs
func (h *SidecarHandler) createTerminatedContainerStatuses(pod *v1.Pod, path, exitCodeMatch, sessionContextMessage string, jidData *JidStruct) []v1.ContainerStatus {
	var containerStatuses []v1.ContainerStatus

	for _, ct := range pod.Spec.Containers {
		exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
		if err != nil {
			log.G(h.Ctx).Error(err)
			continue
		}

		containerStatus := v1.ContainerStatus{
			Name: ct.Name,
			State: v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					StartedAt:  metav1.Time{Time: jidData.StartTime},
					FinishedAt: metav1.Time{Time: jidData.EndTime},
					ExitCode:   exitCode,
				},
			},
			Ready: false,
		}
		containerStatuses = append(containerStatuses, containerStatus)
	}

	return containerStatuses
}

// createWaitingContainerStatuses creates container statuses for waiting jobs
func (h *SidecarHandler) createWaitingContainerStatuses(pod *v1.Pod) []v1.ContainerStatus {
	var containerStatuses []v1.ContainerStatus

	for _, ct := range pod.Spec.Containers {
		containerStatus := v1.ContainerStatus{
			Name:  ct.Name,
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}},
			Ready: false,
		}
		containerStatuses = append(containerStatuses, containerStatus)
	}

	return containerStatuses
}

// createUnknownContainerStatuses creates container statuses for unknown job states
func (h *SidecarHandler) createUnknownContainerStatuses(pod *v1.Pod) []v1.ContainerStatus {
	var containerStatuses []v1.ContainerStatus

	for _, ct := range pod.Spec.Containers {
		containerStatus := v1.ContainerStatus{
			Name:  ct.Name,
			State: v1.ContainerState{},
			Ready: false,
		}
		containerStatuses = append(containerStatuses, containerStatus)
	}

	return containerStatuses
}

// handleJobStateTransition handles job state transitions and timestamp updates
func (h *SidecarHandler) handleJobStateTransition(spanCtx context.Context, stateMatch, uid, path string, timeNow time.Time, w http.ResponseWriter) error {
	jidData := (*h.JIDs)[uid]

	switch stateMatch {
	case "CD", "F", "PR", "ST":
		// Terminal states - set end time if not already set
		if jidData.EndTime.IsZero() {
			jidData.EndTime = timeNow
			return h.writeTimeFile(spanCtx, path, "FinishedAt.time", jidData.EndTime, w)
		}
	case "CG", "R":
		// Running states - set start time if not already set
		if jidData.StartTime.IsZero() {
			jidData.StartTime = timeNow
			return h.writeTimeFile(spanCtx, path, "StartedAt.time", jidData.StartTime, w)
		}
	default:
		// Unknown states - treat as terminated
		if jidData.EndTime.IsZero() {
			jidData.EndTime = timeNow
			return h.writeTimeFile(spanCtx, path, "FinishedAt.time", jidData.EndTime, w)
		}
	}
	return nil
}

// processJobState processes a single job state and returns appropriate container statuses
func (h *SidecarHandler) processJobState(spanCtx context.Context, stateMatch string, pod *v1.Pod, uid, path, exitCodeMatch, sessionContextMessage string, timeNow time.Time, w http.ResponseWriter) ([]v1.ContainerStatus, error) {
	// Handle state transitions first
	if err := h.handleJobStateTransition(spanCtx, stateMatch, uid, path, timeNow, w); err != nil {
		return nil, err
	}

	jidData := (*h.JIDs)[uid]

	switch stateMatch {
	case "CD", "F", "PR", "ST":
		// Terminal states
		return h.createTerminatedContainerStatuses(pod, path, exitCodeMatch, sessionContextMessage, jidData), nil
	case "CG", "R":
		// Running states
		return h.createRunningContainerStatuses(spanCtx, pod, path, jidData), nil
	case "PD", "S":
		// Waiting states
		return h.createWaitingContainerStatuses(pod), nil
	default:
		// Unknown states - treat as terminated
		return h.createTerminatedContainerStatuses(pod, path, exitCodeMatch, sessionContextMessage, jidData), nil
	}
}

// handleJobStatusFromFiles handles the case when squeue returns an error and we need to read from files
func (h *SidecarHandler) handleJobStatusFromFiles(spanCtx context.Context, pod *v1.Pod, path, sessionContextMessage string, w http.ResponseWriter) ([]v1.ContainerStatus, error) {
	var containerStatuses []v1.ContainerStatus

	for _, ct := range pod.Spec.Containers {
		log.G(h.Ctx).Info(sessionContextMessage, "getting exit status from  "+path+"/run-"+ct.Name+".status")
		file, err := os.Open(path + "/run-" + ct.Name + ".status")
		if err != nil {
			h.handleError(spanCtx, w, http.StatusInternalServerError, fmt.Errorf(sessionContextMessage+"unable to retrieve container status: %s", err))
			log.G(h.Ctx).Error()
			return nil, err
		}
		defer file.Close()

		statusb, err := io.ReadAll(file)
		if err != nil {
			h.handleError(spanCtx, w, http.StatusInternalServerError, fmt.Errorf(sessionContextMessage+"unable to read container status: %s", err))
			log.G(h.Ctx).Error()
			return nil, err
		}

		status, err := strconv.Atoi(strings.ReplaceAll(string(statusb), "\n", ""))
		if err != nil {
			h.handleError(spanCtx, w, http.StatusInternalServerError, fmt.Errorf(sessionContextMessage+"unable to convert container status: %s", err))
			log.G(h.Ctx).Error()
			status = 500
		}

		containerStatus := v1.ContainerStatus{
			Name: ct.Name,
			State: v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					ExitCode: int32(status),
				},
			},
			Ready: false,
		}
		containerStatuses = append(containerStatuses, containerStatus)
	}

	return containerStatuses, nil
}
