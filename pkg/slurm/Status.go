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

	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// StatusHandler performs a squeue --me and uses regular expressions to get the running Jobs' status
func (h *SidecarHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "StatusSLURM", trace.WithAttributes(
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

				//log.G(h.Ctx).Info("Pod: " + jid.PodUID + " | JID: " + jid.JID)

				if execReturn.Stderr != "" {
					span.AddEvent("squeue returned error " + execReturn.Stderr + " for Job " + (*h.JIDs)[uid].JID + ".\nGetting status from files")
					log.G(h.Ctx).Error(sessionContextMessage, "ERR: ", execReturn.Stderr)
					for _, ct := range pod.Spec.Containers {
						log.G(h.Ctx).Info(sessionContextMessage, "getting exit status from  "+path+"/"+ct.Name+".status")
						file, err := os.Open(path + "/" + ct.Name + ".status")
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
					statePattern := `(CD|CG|F|PD|PR|R|S|ST)`
					stateRe := regexp.MustCompile(statePattern)
					stateMatch := stateRe.FindString(execReturn.Stdout)

					// If the job is not in terminal state, the exit code has no meaning, however squeue returns 0 for exit code in this case. Just ignore the value.
					// Magic REGEX that matches any number from 0 to 255 included. Eg: match 2, 255, does not match 256, 02, -1.
					// Adds whitespace because otherwise it will take too few letter. Eg: for "123", it will take only "1". With \s, it will take "123 ".
					// Then we only keep the number part, not the last space.
					exitCodePattern := `([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\s`
					exitCodeRe := regexp.MustCompile(exitCodePattern)
					// Eg: exitCodeMatchSlice = "123 "
					exitCodeMatchSlice := exitCodeRe.FindStringSubmatch(execReturn.Stdout)
					// Only keep the number part. Eg: exitCodeMatch = "123"
					exitCodeMatch := exitCodeMatchSlice[1]

					//log.G(h.Ctx).Info("JID: " + (*h.JIDs)[uid].JID + " | Status: " + stateMatch + " | Pod: " + pod.Name + " | UID: " + string(pod.UID))
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
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}}}, Ready: true}
							containerStatuses = append(containerStatuses, containerStatus)
						}
						resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
					case "F":
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
							containerStatus := v1.ContainerStatus{Name: ct.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.Time{Time: (*h.JIDs)[uid].StartTime}}}, Ready: true}
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
