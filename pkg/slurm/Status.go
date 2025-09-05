package slurm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	commonIL "github.com/intertwin-eu/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

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

	// If no pods are requested, return sinfo -s output
	if len(req) == 0 {
		if err := h.handleEmptyPodList(spanCtx, w); err != nil {
			return
		}
		return
	}

	if timeNow.Sub(timer) >= time.Second*10 {
		cmd := []string{"--me"}
		shell := exec.ExecTask{
			Command: h.Config.Squeuepath,
			Args:    cmd,
			Shell:   true,
		}
		execReturn, err := shell.Execute()
		if err != nil {
			log.G(h.Ctx).Error("Failed to execute shell command: ", err)
		}
		execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

		if execReturn.Stderr != "" {
			statusCode = http.StatusInternalServerError
			h.handleError(spanCtx, w, statusCode, errors.New(sessionContextMessage+"unable to retrieve job status: "+execReturn.Stderr))
			return
		}

		for _, pod := range req {
			var containerStatuses []v1.ContainerStatus
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
				execReturn, err := shell.Execute()
				if err != nil {
					log.G(h.Ctx).Error("Failed to execute shell command: ", err)
				}
				timeNow = time.Now()

				// log.G(h.Ctx).Info("Pod: " + jid.PodUID + " | JID: " + jid.JID)

				if execReturn.Stderr != "" {
					span.AddEvent("squeue returned error " + execReturn.Stderr + " for Job " + (*h.JIDs)[uid].JID + ".\nGetting status from files")
					log.G(h.Ctx).Error(sessionContextMessage, "ERR: ", execReturn.Stderr)

					containerStatuses, err = h.handleJobStatusFromFiles(spanCtx, pod, path, sessionContextMessage, w)
					if err != nil {
						return
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

					// log.G(h.Ctx).Info("JID: " + (*h.JIDs)[uid].JID + " | Status: " + stateMatch + " | Pod: " + pod.Name + " | UID: " + string(pod.UID))
					log.G(h.Ctx).Infof("%sJID: %s | Status: %s | Job exit code (if applicable): %s | Pod: %s | UID: %s", sessionContextMessage, (*h.JIDs)[uid].JID, stateMatch, exitCodeMatch, pod.Name, string(pod.UID))

					containerStatuses, err = h.processJobState(spanCtx, stateMatch, pod, uid, path, exitCodeMatch, sessionContextMessage, timeNow, w)
					if err != nil {
						return
					}

					resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: string(pod.UID), PodNamespace: pod.Namespace, Containers: containerStatuses})
				}
			} else {
				containerStatuses = h.createUnknownContainerStatuses(pod)
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
		if _, err := w.Write([]byte("Some errors occurred deleting containers. Check SLURM Sidecar's logs")); err != nil {
			log.G(h.Ctx).Error("Failed to write error response: ", err)
		}
	} else {
		bodyBytes, err := json.Marshal(resp)
		if err != nil {
			h.handleError(spanCtx, w, statusCode, err)
			return
		}
		if _, err := w.Write(bodyBytes); err != nil {
			log.G(h.Ctx).Error("Failed to write response body: ", err)
		}
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
