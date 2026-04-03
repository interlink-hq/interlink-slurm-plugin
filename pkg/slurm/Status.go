package slurm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// Reason and message strings surfaced in ContainerStateTerminated for SLURM-specific
// termination causes.  Defining them as constants lets tests reference the actual values
// used at runtime rather than independently-maintained string literals.
const (
	// ReasonSlurmJobTimeout is set on every container when the SLURM job was cancelled
	// because it reached its configured time limit (state "TO").  The Virtual Kubelet
	// propagates this reason to the pod status, setting the pod phase to Failed so that
	// the pod's restart policy or owning controller (Deployment, Job, …) can act on it.
	ReasonSlurmJobTimeout  = "SlurmJobTimeout"
	MessageSlurmJobTimeout = "SLURM job reached its time limit and was terminated"

	// ReasonOOMKilled matches the Kubernetes convention for out-of-memory terminations
	// (state "OOM") so that existing tooling can identify OOM-killed containers.
	ReasonOOMKilled  = "OOMKilled"
	MessageOOMKilled = "SLURM job was killed due to out-of-memory condition"

	// SlurmStatePattern is the regex alternation used to extract the SLURM job state
	// from a squeue output line.  Longer alternatives (e.g. ST, OOM) are listed before
	// shorter prefixes (S) so the regex engine picks the correct token.
	SlurmStatePattern = `(CD|CG|F|OOM|PD|PR|R|ST|S|TO)`

	// SlurmExitCodePattern is the regex used to extract the numeric exit/signal code
	// from a squeue output line (format "<exit>:<signal>  <state>").
	SlurmExitCodePattern = `([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\s`
)

// terminatedContainerStatus builds a ContainerStatus with a Terminated state for a
// container that has finished (successfully or otherwise).  reason and message are
// surfaced verbatim in ContainerStateTerminated; pass empty strings when no
// named reason is needed (e.g. for ordinary CD/F/ST/default cases).
func terminatedContainerStatus(ctName string, startTime, finishTime time.Time, exitCode int32, reason, message string) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name: ctName,
		State: v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				StartedAt:  metav1.Time{Time: startTime},
				FinishedAt: metav1.Time{Time: finishTime},
				ExitCode:   exitCode,
				Reason:     reason,
				Message:    message,
			},
		},
		Ready: false,
	}
}

// StatusHandler performs a squeue --me and uses regular expressions to get the running Jobs' status
func (h *SidecarHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "Status", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	// For debugging purpose, when we have many kubectl logs, we can differentiate each one.
	sessionContext := GetSessionContext(r)
	sessionContextMessage := GetSessionContextMessage(sessionContext)

	var req []*v1.Pod
	var resp []commonIL.PodStatus
	statusCode := http.StatusOK
	log.G(h.Ctx).Info("Slurm Sidecar: received GetStatus call")
	timeNow := time.Now()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	err = json.Unmarshal(bodyBytes, &req)
	if err != nil {
		statusCode = http.StatusInternalServerError
		h.handleError(spanCtx, w, statusCode, err)
		return
	}

	// If no pods are requested, return cluster resource availability as JSON.
	// This path is triggered by the interlink-api ping call so that the virtual
	// kubelet can update the node's advertised capacity from the SLURM cluster state.
	if len(req) == 0 {
		resources, err := h.getClusterResources()
		if err != nil {
			log.G(h.Ctx).Warning("Failed to query SLURM cluster resources: ", err)
			statusCode = http.StatusInternalServerError
			h.handleError(spanCtx, w, statusCode, err)
			return
		}

		resourceBytes, err := json.Marshal(resources)
		if err != nil {
			statusCode = http.StatusInternalServerError
			h.handleError(spanCtx, w, statusCode, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(resourceBytes)
		log.G(h.Ctx).Info("Returned cluster resource information for empty pod list (ping)")
		return
	}

	if timeNow.Sub(timer) >= time.Second*10 {
		cmd := []string{"--me"}
		shell := exec.ExecTask{
			Command: h.Config.Squeuepath,
			Args:    cmd,
			Shell:   true,
		}
		execReturn, _ := shell.Execute()
		execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

		if execReturn.Stderr != "" {
			statusCode = http.StatusInternalServerError
			h.handleError(spanCtx, w, statusCode, errors.New(sessionContextMessage+"unable to retrieve job status: "+execReturn.Stderr))
			return
		}

		for _, pod := range req {
			containerStatuses := []v1.ContainerStatus{}
			uid := string(pod.UID)
			path := h.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID)

			if checkIfJidExists(spanCtx, (h.JIDs), uid) {
				// Eg of output: "R 0"
				// With test, exit_code is better than DerivedEC, because for canceled jobs, it gives 15 while DerivedEC gives 0.
				// states=all or else some jobs are hidden, then it is impossible to get job exit code.
				cmd := []string{"--noheader", "-a", "--states=all", "-O", "exit_code,StateCompact", "-j ", (*h.JIDs)[uid].JID}
				shell := exec.ExecTask{
					Command: h.Config.Squeuepath,
					Args:    cmd,
					// true to be able to add prefix to squeue, but this is ugly
					Shell: true,
				}
				execReturn, _ := shell.Execute()
				timeNow = time.Now()

				// log.G(h.Ctx).Info("Pod: " + jid.PodUID + " | JID: " + jid.JID)

				if execReturn.Stderr != "" {
					span.AddEvent("squeue returned error " + execReturn.Stderr + " for Job " + (*h.JIDs)[uid].JID + ".\nGetting status from files")
					log.G(h.Ctx).Error(sessionContextMessage, "ERR: ", execReturn.Stderr)
					for _, ct := range pod.Spec.Containers {
						log.G(h.Ctx).Info(sessionContextMessage, "getting exit status from  "+path+"/run-"+ct.Name+".status")
						file, err := os.Open(path + "/run-" + ct.Name + ".status")
						if err != nil {
							statusCode = http.StatusInternalServerError
							h.handleError(spanCtx, w, statusCode, fmt.Errorf(sessionContextMessage+"unable to retrieve container status: %s", err))
							log.G(h.Ctx).Error()
							return
						}
						defer file.Close()
						statusb, err := io.ReadAll(file)
						if err != nil {
							statusCode = http.StatusInternalServerError
							h.handleError(spanCtx, w, statusCode, fmt.Errorf(sessionContextMessage+"unable to read container status: %s", err))
							log.G(h.Ctx).Error()
							return
						}

						status, err := strconv.Atoi(strings.Replace(string(statusb), "\n", "", -1))
						if err != nil {
							statusCode = http.StatusInternalServerError
							h.handleError(spanCtx, w, statusCode, fmt.Errorf(sessionContextMessage+"unable to convert container status: %s", err))
							log.G(h.Ctx).Error()
							status = 500
						}

						containerStatuses = append(
							containerStatuses,
							v1.ContainerStatus{
								Name: ct.Name,
								State: v1.ContainerState{
									Terminated: &v1.ContainerStateTerminated{
										ExitCode: int32(status),
									},
								},
								Ready: false,
							},
						)

					}

					resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
				} else {
					statePattern := SlurmStatePattern
					stateRe := regexp.MustCompile(statePattern)
					stateMatch := stateRe.FindString(execReturn.Stdout)

					// If the job is not in terminal state, the exit code has no meaning, however squeue returns 0 for exit code in this case. Just ignore the value.
					// Magic REGEX that matches any number from 0 to 255 included. Eg: match 2, 255, does not match 256, 02, -1.
					// Adds whitespace because otherwise it will take too few letter. Eg: for "123", it will take only "1". With \s, it will take "123 ".
					// Then we only keep the number part, not the last space.
					exitCodePattern := SlurmExitCodePattern
					exitCodeRe := regexp.MustCompile(exitCodePattern)
					// Eg: exitCodeMatchSlice = "123 "
					exitCodeMatchSlice := exitCodeRe.FindStringSubmatch(execReturn.Stdout)
					// Only keep the number part. Eg: exitCodeMatch = "123"
					exitCodeMatch := exitCodeMatchSlice[1]

					// log.G(h.Ctx).Info("JID: " + (*h.JIDs)[uid].JID + " | Status: " + stateMatch + " | Pod: " + pod.Name + " | UID: " + string(pod.UID))
					log.G(h.Ctx).Infof("%sJID: %s | Status: %s | Job exit code (if applicable): %s | Pod: %s | UID: %s", sessionContextMessage, (*h.JIDs)[uid].JID, stateMatch, exitCodeMatch, pod.Name, string(pod.UID))

					switch stateMatch {
					case "CD":
						if (*h.JIDs)[uid].EndTime.IsZero() {
							(*h.JIDs)[uid].EndTime = timeNow
							f, err := os.Create(path + "/FinishedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}, FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime}, ExitCode: int32(exitCode)}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "CG":
						if (*h.JIDs)[uid].StartTime.IsZero() {
							(*h.JIDs)[uid].StartTime = timeNow
							f, err := os.Create(path + "/StartedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].StartTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							// Check probe status for container readiness
							readinessCount, _, startupCount, err := loadProbeMetadata(path, ct.Name)
							isReady := true
							if err != nil {
								log.G(h.Ctx).Debug("Failed to load probe metadata for container ", ct.Name, ": ", err)
							} else {
								// Container is ready only if:
								// 1. Startup probes have completed (or none configured), AND
								// 2. Readiness probes have succeeded (or none configured)
								startupComplete := checkContainerStartupComplete(spanCtx, h.Config, path, ct.Name, startupCount)
								readinessOK := checkContainerReadiness(spanCtx, h.Config, path, ct.Name, readinessCount)
								isReady = startupComplete && readinessOK
							}

							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}}}, Ready: isReady}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "F":
						// patch to fix Leonardo temporary F status after submit
						_, err := os.Stat(path + "/FinishedAt.time")
						if (*h.JIDs)[uid].EndTime.IsZero() && errors.Is(err, os.ErrNotExist) {
							(*h.JIDs)[uid].EndTime = timeNow
							f, err := os.Create(path + "/FinishedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}, FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime}, ExitCode: exitCode}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "PD":
						for _, ct := range pod.Spec.Containers {
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "PR":
						if (*h.JIDs)[uid].EndTime.IsZero() {
							(*h.JIDs)[uid].EndTime = timeNow
							f, err := os.Create(path + "/FinishedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}, FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime}, ExitCode: exitCode}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "R":
						if (*h.JIDs)[uid].StartTime.IsZero() {
							(*h.JIDs)[uid].StartTime = timeNow
							f, err := os.Create(path + "/StartedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].StartTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							// Check probe status for container readiness
							readinessCount, livenessCount, startupCount, err := loadProbeMetadata(path, ct.Name)
							isReady := false
							if err != nil {
								log.G(h.Ctx).Debug("Failed to load probe metadata for container ", ct.Name, ": ", err)
							} else {
								// Container is ready only if:
								// 1. Startup probes have completed (or none configured), AND
								// 2. Readiness probes have succeeded (or none configured)
								startupComplete := checkContainerStartupComplete(spanCtx, h.Config, path, ct.Name, startupCount)
								readinessOK := checkContainerReadiness(spanCtx, h.Config, path, ct.Name, readinessCount)
								livenessOK := checkContainerLiveness(spanCtx, h.Config, path, ct.Name, livenessCount)
								isReady = startupComplete && readinessOK && livenessOK

								log.G(h.Ctx).Debugf("%sContainer %s: startupComplete=%v, readinessOK=%v, liveneessOK=%v, isReady=%v", sessionContextMessage, ct.Name, startupComplete, readinessOK, livenessOK, isReady)
							}

							var containerStatus v1.ContainerStatus

							if isReady {
								containerStatus = v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}}}, Ready: isReady}
							} else {
								containerStatus = v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "Waiting for probes to be ready."}}, Ready: isReady}
							}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "S":
						for _, ct := range pod.Spec.Containers {
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "ST":
						if (*h.JIDs)[uid].EndTime.IsZero() {
							(*h.JIDs)[uid].EndTime = timeNow
							f, err := os.Create(path + "/FinishedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}, FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime}, ExitCode: exitCode}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "TO":
						// SLURM job reached its time limit. Report containers as Terminated with
						// reason SlurmJobTimeout so the Virtual Kubelet sets the pod phase to Failed
						// and the pod's restart/owning-controller policy handles resubmission.
						if (*h.JIDs)[uid].EndTime.IsZero() {
							(*h.JIDs)[uid].EndTime = timeNow
							finishedAtStr := (*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST")
							if err := os.WriteFile(path+"/FinishedAt.time", []byte(finishedAtStr), 0o644); err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
						}
						log.G(h.Ctx).Infof("%sSLURM job %s reached time limit (pod %s/%s); reporting containers as terminated",
							sessionContextMessage, (*h.JIDs)[uid].JID, pod.Namespace, pod.Name)
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := terminatedContainerStatus(ct.Name, (*h.JIDs)[uid].StartTime, (*h.JIDs)[uid].EndTime, exitCode, ReasonSlurmJobTimeout, MessageSlurmJobTimeout)
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "OOM":
						// SLURM job was killed due to an out-of-memory condition.
						if (*h.JIDs)[uid].EndTime.IsZero() {
							(*h.JIDs)[uid].EndTime = timeNow
							finishedAtStr := (*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST")
							if err := os.WriteFile(path+"/FinishedAt.time", []byte(finishedAtStr), 0o644); err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
						}
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := terminatedContainerStatus(ct.Name, (*h.JIDs)[uid].StartTime, (*h.JIDs)[uid].EndTime, exitCode, ReasonOOMKilled, MessageOOMKilled)
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					default:
						if (*h.JIDs)[uid].EndTime.IsZero() {
							(*h.JIDs)[uid].EndTime = timeNow
							f, err := os.Create(path + "/FinishedAt.time")
							if err != nil {
								statusCode = http.StatusInternalServerError
								h.handleError(spanCtx, w, statusCode, err)
								return
							}
							f.WriteString((*h.JIDs)[uid].EndTime.Format("2006-01-02 15:04:05.999999999 -0700 MST"))
						}
						for _, ct := range pod.Spec.Containers {
							exitCode, err := getExitCode(h.Ctx, path, ct.Name, exitCodeMatch, sessionContextMessage)
							if err != nil {
								log.G(h.Ctx).Error(err)
								continue
							}
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}, FinishedAt: metav1.Time{Time: (*h.JIDs)[uid].EndTime}, ExitCode: exitCode}}, Ready: false}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					}
				}
			} else {
				for _, ct := range pod.Spec.Containers {
					containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{}, Ready: false}
					containerStatuses = append(containerStatuses, containerStatus)
				}
				resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
			}

		}
		cachedStatus = resp
		timer = time.Now()
	} else {
		log.G(h.Ctx).Debug(sessionContextMessage, "Cached status")
		resp = cachedStatus
	}

	log.G(h.Ctx).Debug(resp)

	w.WriteHeader(statusCode)
	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred deleting containers. Check SLURM Sidecar's logs"))
	} else {
		bodyBytes, err := json.Marshal(resp)
		if err != nil {
			h.handleError(spanCtx, w, statusCode, err)
			return
		}
		w.Write(bodyBytes)
	}
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

// getClusterResources queries SLURM for the current resource usage of the cluster and
// returns a NodeResources summary.  It first attempts to use `sinfo --json` (available
// in SLURM >= 20.11) to get accurate per-node allocation data.  When the JSON output
// is unavailable or cannot be parsed it falls back to `sinfo --noheader --format=…`
// text parsing, which reports totals but cannot determine per-node CPU allocation.
func (h *SidecarHandler) getClusterResources() (NodeResources, error) {
	resources, err := h.getClusterResourcesFromJSON()
	if err != nil {
		log.G(h.Ctx).Debugf("sinfo --json unavailable (%v), falling back to text parsing", err)
		return h.getClusterResourcesFromText()
	}
	return resources, nil
}

// getClusterResourcesFromJSON uses `sinfo --json` to obtain per-node resource data.
func (h *SidecarHandler) getClusterResourcesFromJSON() (NodeResources, error) {
	shell := exec.ExecTask{
		Command: h.Config.Sinfopath,
		Args:    []string{"--json"},
		Shell:   true,
	}
	execReturn, err := shell.Execute()
	if err != nil {
		return NodeResources{}, fmt.Errorf("sinfo --json execution failed: %w", err)
	}
	if execReturn.Stderr != "" {
		return NodeResources{}, fmt.Errorf("sinfo --json stderr: %s", execReturn.Stderr)
	}
	return parseClusterResourcesFromJSON(execReturn.Stdout)
}

// getClusterResourcesFromText uses `sinfo --noheader --format=…` to obtain per-node
// resource totals.  Because plain-text sinfo output does not expose per-node allocated
// CPU counts, CPUUsedCores is left at zero.
func (h *SidecarHandler) getClusterResourcesFromText() (NodeResources, error) {
	// %c = CPUs on node, %m = real memory (MB), %e = free memory (MB).
	// Field separator is a pipe so that values with spaces still parse correctly.
	shell := exec.ExecTask{
		Command: h.Config.Sinfopath,
		Args:    []string{"--noheader", "--format=%c|%m|%e"},
		Shell:   true,
	}
	execReturn, err := shell.Execute()
	if err != nil {
		return NodeResources{}, fmt.Errorf("sinfo execution failed: %w", err)
	}
	if execReturn.Stderr != "" {
		return NodeResources{}, fmt.Errorf("sinfo stderr: %s", execReturn.Stderr)
	}
	return parseClusterResourcesFromText(execReturn.Stdout)
}

// parseClusterResourcesFromJSON parses the stdout of `sinfo --json` into a
// NodeResources value.  It is a standalone function so it can be exercised in tests
// without executing an actual sinfo binary.
func parseClusterResourcesFromJSON(stdout string) (NodeResources, error) {
	var parsed slurmNodeList
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return NodeResources{}, fmt.Errorf("sinfo --json parse error: %w", err)
	}
	if len(parsed.Nodes) == 0 {
		return NodeResources{}, fmt.Errorf("sinfo --json returned no nodes")
	}

	var totalCPU, allocCPU, totalMemMB, usedMemMB int64
	for _, node := range parsed.Nodes {
		totalCPU += node.CPUs
		allocCPU += node.AllocCPUs
		totalMemMB += node.RealMemory
		// Prefer explicit alloc_memory when available; fall back to real_memory - free_memory.
		if node.AllocMemory > 0 {
			usedMemMB += node.AllocMemory
		} else {
			usedMemMB += node.RealMemory - node.FreeMemory
		}
	}

	return NodeResources{
		CPUTotalCores:    totalCPU,
		CPUUsedCores:     allocCPU,
		MemoryTotalBytes: totalMemMB * 1024 * 1024,
		MemoryUsedBytes:  usedMemMB * 1024 * 1024,
	}, nil
}

// parseClusterResourcesFromText parses the stdout of
// `sinfo --noheader --format=%c|%m|%e` into a NodeResources value.  Lines that
// cannot be parsed are silently skipped.  It is a standalone function so it can be
// exercised in tests without executing an actual sinfo binary.
func parseClusterResourcesFromText(stdout string) (NodeResources, error) {
	var totalCPU, totalMemMB, freeMemMB int64
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		cpu, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			continue
		}
		mem, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		free, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil {
			continue
		}
		totalCPU += cpu
		totalMemMB += mem
		freeMemMB += free
	}

	return NodeResources{
		CPUTotalCores:    totalCPU,
		CPUUsedCores:     0, // not derivable from plain-text sinfo output (requires --json or squeue)
		MemoryTotalBytes: totalMemMB * 1024 * 1024,
		MemoryUsedBytes:  (totalMemMB - freeMemMB) * 1024 * 1024,
	}, nil
}
