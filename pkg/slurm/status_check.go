package slurm

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

var (
// cachedStatus and timer are declared in prepare.go for StatusHandler compatibility
)

// StatusResult holds the result of querying pod statuses from SLURM.
// This is a reusable method extracted from StatusHandler for use by the BatchSystem adapter.
func (h *SidecarHandler) GetPodStatuses(ctx context.Context, pods []*v1.Pod) ([]commonIL.PodStatus, error) {
	tracer := otel.Tracer("interlink-slurm")
	_, span := tracer.Start(ctx, "GetPodStatuses", trace.WithAttributes(
		attribute.Int("pod_count", len(pods)),
	))
	defer span.End()

	var resp []commonIL.PodStatus
	timeNow := time.Now()

	// If no pods are requested, return empty array
	if len(pods) == 0 {
		return resp, nil
	}

	// Check cache (10 second TTL)
	if timeNow.Sub(timer) < time.Second*10 && len(cachedStatus) > 0 {
		log.G(ctx).Debug("Using cached status")
		return cachedStatus, nil
	}

	// Fetch current SLURM status
	cmd := []string{"--me"}
	shell := exec.ExecTask{
		Command: h.Config.Squeuepath,
		Args:    cmd,
		Shell:   true,
	}
	execReturn, _ := shell.Execute()
	execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

	if execReturn.Stderr != "" {
		log.G(ctx).Error("Failed to retrieve job status from SLURM: ", execReturn.Stderr)
		// Return cached status if available, otherwise continue with empty
		if len(cachedStatus) > 0 {
			return cachedStatus, nil
		}
	}

	// Process each pod
	for _, pod := range pods {
		containerStatuses := []v1.ContainerStatus{}
		uid := string(pod.UID)
		path := h.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID)

		if checkIfJidExists(ctx, h.JIDs, uid) {
			// Query specific job for detailed status
			cmd := []string{"--noheader", "-a", "--states=all", "-O", "exit_code,StateCompact", "-j ", (*h.JIDs)[uid].JID}
			shell := exec.ExecTask{
				Command: h.Config.Squeuepath,
				Args:    cmd,
				Shell:   true,
			}
			execReturn, _ := shell.Execute()
			timeNow = time.Now()

			if execReturn.Stderr != "" {
				// Fallback: get status from files
				log.G(ctx).Error("squeue error for job ", (*h.JIDs)[uid].JID, ": ", execReturn.Stderr, ". Getting status from files.")
				for _, ct := range pod.Spec.Containers {
					status, err := h.getContainerStatusFromFile(ctx, path, ct.Name)
					if err != nil {
						log.G(ctx).Error("Failed to get container status for ", ct.Name, ": ", err)
						continue
					}
					containerStatuses = append(containerStatuses, status)
				}
				resp = append(resp, commonIL.PodStatus{
					PodName:      pod.Name,
					PodUID:       string(pod.UID),
					PodNamespace: pod.Namespace,
					Containers:   containerStatuses,
				})
				continue
			}

			// Parse SLURM state
			statePattern := `(CD|CG|F|PD|PR|R|S|ST)`
			stateRe := regexp.MustCompile(statePattern)
			stateMatch := stateRe.FindString(execReturn.Stdout)

			// Parse exit code
			exitCodePattern := `([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\s`
			exitCodeRe := regexp.MustCompile(exitCodePattern)
			exitCodeMatchSlice := exitCodeRe.FindStringSubmatch(execReturn.Stdout)
			exitCodeMatch := ""
			if len(exitCodeMatchSlice) > 1 {
				exitCodeMatch = exitCodeMatchSlice[1]
			}

			log.G(ctx).Infof("JID: %s | Status: %s | Exit Code: %s | Pod: %s | UID: %s",
				(*h.JIDs)[uid].JID, stateMatch, exitCodeMatch, pod.Name, string(pod.UID))

			// Map SLURM state to Kubernetes container status
			h.processPodStatus(ctx, pod, uid, path, stateMatch, exitCodeMatch, timeNow, &containerStatuses)
			resp = append(resp, commonIL.PodStatus{
				PodName:      pod.Name,
				PodUID:       string(pod.UID),
				PodNamespace: pod.Namespace,
				Containers:   containerStatuses,
			})
		} else {
			// Job not found - return empty status
			for _, ct := range pod.Spec.Containers {
				containerStatuses = append(containerStatuses, v1.ContainerStatus{
					Name:  ct.Name,
					State: v1.ContainerState{},
					Ready: false,
				})
			}
			resp = append(resp, commonIL.PodStatus{
				PodName:      pod.Name,
				PodUID:       string(pod.UID),
				PodNamespace: pod.Namespace,
				Containers:   containerStatuses,
			})
		}
	}

	// Update cache
	cachedStatus = resp
	timer = timeNow

	return resp, nil
}

// processPodStatus processes a single pod's SLURM state and populates container statuses
func (h *SidecarHandler) processPodStatus(
	ctx context.Context,
	pod *v1.Pod,
	uid string,
	path string,
	stateMatch string,
	exitCodeMatch string,
	timeNow time.Time,
	containerStatuses *[]v1.ContainerStatus,
) {
	sessionContextMessage := "SLURM: "

	switch stateMatch {
	case "CD": // Completed
		h.updateJobEndTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			exitCode, err := getExitCode(ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
			if err != nil {
				log.G(ctx).Error(err)
				continue
			}
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						StartedAt:  metav1.Time{Time: (*h.JIDs)[uid].StartTime},
						FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime},
						ExitCode:   int32(exitCode),
					},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "CG": // Completing
		h.updateJobStartTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			readinessCount, _, _, _ := loadProbeMetadata(path, ct.Name)
			isReady := checkContainerReadiness(ctx, h.Config, path, ct.Name, readinessCount)
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Running: &v1.ContainerStateRunning{
						StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime},
					},
				},
				Ready: isReady,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "F": // Failed
		h.updateJobEndTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			exitCode, err := getExitCode(ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
			if err != nil {
				log.G(ctx).Error(err)
				continue
			}
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						StartedAt:  metav1.Time{Time: (*h.JIDs)[uid].StartTime},
						FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime},
						ExitCode:   int32(exitCode),
					},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "PD": // Pending
		for _, ct := range pod.Spec.Containers {
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Waiting: &v1.ContainerStateWaiting{},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "PR": // Preempted
		h.updateJobEndTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			exitCode, err := getExitCode(ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
			if err != nil {
				log.G(ctx).Error(err)
				continue
			}
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						StartedAt:  metav1.Time{Time: (*h.JIDs)[uid].StartTime},
						FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime},
						ExitCode:   int32(exitCode),
					},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "R": // Running
		h.updateJobStartTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			readinessCount, _, _, _ := loadProbeMetadata(path, ct.Name)
			isReady := checkContainerReadiness(ctx, h.Config, path, ct.Name, readinessCount)
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Running: &v1.ContainerStateRunning{
						StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime},
					},
				},
				Ready: isReady,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "S": // Suspended
		for _, ct := range pod.Spec.Containers {
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Waiting: &v1.ContainerStateWaiting{},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	case "ST": // Stopped
		h.updateJobEndTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			exitCode, err := getExitCode(ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
			if err != nil {
				log.G(ctx).Error(err)
				continue
			}
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						StartedAt:  metav1.Time{Time: (*h.JIDs)[uid].StartTime},
						FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime},
						ExitCode:   int32(exitCode),
					},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}

	default:
		// Unknown state - treat as terminated
		h.updateJobEndTime(uid, path, timeNow)
		for _, ct := range pod.Spec.Containers {
			exitCode, err := getExitCode(ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
			if err != nil {
				log.G(ctx).Error(err)
				continue
			}
			status := v1.ContainerStatus{
				Name: ct.Name,
				State: v1.ContainerState{
					Terminated: &v1.ContainerStateTerminated{
						StartedAt:  metav1.Time{Time: (*h.JIDs)[uid].StartTime},
						FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime},
						ExitCode:   int32(exitCode),
					},
				},
				Ready: false,
			}
			*containerStatuses = append(*containerStatuses, status)
		}
	}
}

// getContainerStatusFromFile retrieves container status from a file (fallback when SLURM query fails)
func (h *SidecarHandler) getContainerStatusFromFile(ctx context.Context, path string, containerName string) (v1.ContainerStatus, error) {
	statusPath := path + "/run-" + containerName + ".status"

	file, err := os.Open(statusPath)
	if err != nil {
		// Try init container path
		statusPath = path + "/init-" + containerName + ".status"
		file, err = os.Open(statusPath)
		if err != nil {
			return v1.ContainerStatus{}, err
		}
	}
	defer file.Close()

	statusb, err := os.ReadFile(statusPath)
	if err != nil {
		return v1.ContainerStatus{}, err
	}

	status, err := strconv.Atoi(strings.Replace(string(statusb), "\n", "", -1))
	if err != nil {
		return v1.ContainerStatus{}, err
	}

	return v1.ContainerStatus{
		Name: containerName,
		State: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode: int32(status),
			},
		},
		Ready: false,
	}, nil
}

// updateJobEndTime updates the job end time and writes to file
func (h *SidecarHandler) updateJobEndTime(uid string, path string, timeNow time.Time) {
	if (*h.JIDs)[uid].EndTime.IsZero() {
		(*h.JIDs)[uid].EndTime = timeNow
		f, err := os.Create(path + "/FinishedAt.time")
		if err != nil {
			log.G(h.Ctx).Error("Failed to write finish time: ", err)
			return
		}
		defer f.Close()
		f.WriteString((*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	}
}

// updateJobStartTime updates the job start time and writes to file
func (h *SidecarHandler) updateJobStartTime(uid string, path string, timeNow time.Time) {
	if (*h.JIDs)[uid].StartTime.IsZero() {
		(*h.JIDs)[uid].StartTime = timeNow
		f, err := os.Create(path + "/StartedAt.time")
		if err != nil {
			log.G(h.Ctx).Error("Failed to write start time: ", err)
			return
		}
		defer f.Close()
		f.WriteString((*h.JIDs)[uid].StartTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
	}
}
