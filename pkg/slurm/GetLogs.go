package slurm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// Logs in follow mode (get logs until the death of the container) with "kubectl -f".
func (h *SidecarHandler) GetLogsFollowMode(spanCtx context.Context, podUid string, w http.ResponseWriter, r *http.Request, path string, req commonIL.LogStruct, containerOutputPath string, containerOutput []byte, sessionContext string) error {
	// Follow until this file exist, that indicates the end of container, thus the end of following.
	containerStatusPath := path + "/" + req.ContainerName + ".status"
	// Get the offset of what we read.
	containerOutputLastOffset := len(containerOutput)
	sessionContextMessage := GetSessionContextMessage(sessionContext)
	log.G(h.Ctx).Debug(sessionContextMessage, "Check container status", containerStatusPath, " with current length/offset: ", containerOutputLastOffset)

	var containerOutputFd *os.File
	var err error
	for {
		containerOutputFd, err = os.Open(containerOutputPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Case the file does not exist yet, we loop until it exist.
				notFoundMsg := sessionContextMessage + "Cannot open in follow mode the container logs " + containerOutputPath + " because it does not exist yet, sleeping before retrying..."
				log.G(h.Ctx).Debug(notFoundMsg)
				// Warning: if we don't write anything to body before 30s, there will be a timeout in VK to API DoReq(), thus we send an informational message that the log is not ready.
				w.Write([]byte(notFoundMsg))
				// Flush otherwise it will be as if nothing was written.
				if f, ok := w.(http.Flusher); ok {
					log.G(h.Ctx).Debug(sessionContextMessage, "wrote file not found yet, now flushing this message...")
					f.Flush()
				} else {
					log.G(h.Ctx).Error(sessionContextMessage, "wrote file not found but could not flush because server does not support Flusher.")
				}
				time.Sleep(4 * time.Second)
				continue
			} else {
				// Case unknown error.
				errWithContext := fmt.Errorf(sessionContextMessage+"could not open file to follow logs at %s error type: %s error: %w", containerOutputPath, fmt.Sprintf("%#v", err), err)
				log.G(h.Ctx).Error(errWithContext)
				w.Write([]byte(errWithContext.Error()))
				return err
			}
		}
		// File exist.
		log.G(h.Ctx).Debug(sessionContextMessage, "opened for follow mode the container logs ", containerOutputPath)
		break
	}
	defer containerOutputFd.Close()

	// We follow only from after what is already read.
	_, err = containerOutputFd.Seek(int64(containerOutputLastOffset), 0)
	if err != nil {
		errWithContext := fmt.Errorf(sessionContextMessage+"error during Seek() of GetLogsFollowMode() in GetLogsHandler of file %s offset %d type: %s %w", containerOutputPath, containerOutputLastOffset, fmt.Sprintf("%#v", err), err)
		w.Write([]byte(errWithContext.Error()))
		return errWithContext
	}

	containerOutputReader := bufio.NewReader(containerOutputFd)

	bufferBytes := make([]byte, 4096)

	// Looping until we get end of job.
	// TODO: handle the Ctrl+C of kubectl logs.
	var isContainerDead bool = false
	for {
		n, errRead := containerOutputReader.Read(bufferBytes)
		if errRead != nil && errRead != io.EOF {
			// Error during read.
			h.logErrorVerbose(sessionContextMessage+"error doing Read() of GetLogsFollowMode", h.Ctx, w, errRead)
			return errRead
		}
		// Write ASAP what we could read of it.
		_, err = w.Write(bufferBytes[:n])
		if err != nil {
			h.logErrorVerbose(sessionContextMessage+"error doing Write() of GetLogsFollowMode", h.Ctx, w, err)
			return err
		}

		// Flush otherwise it will take time to appear in kubectl logs.
		if f, ok := w.(http.Flusher); ok {
			log.G(h.Ctx).Debug(sessionContextMessage, "wrote some logs, now flushing...")
			f.Flush()
		} else {
			log.G(h.Ctx).Error(sessionContextMessage, "wrote some logs but could not flush because server does not support Flusher.")
		}

		if errRead != nil {
			if errRead == io.EOF {
				// Nothing more to read, but in follow mode, is the container still alive?
				if isContainerDead {
					// Container already marked as dead, and we tried to get logs one last time. Exiting the loop.
					log.G(h.Ctx).Info(sessionContextMessage, "Container was found dead and no more logs are found at this step, exiting following mode...")
					break
				}
				// Checking if container is dead (meaning the job ID is not in context anymore, OR if the status file exist).
				if !checkIfJidExists(spanCtx, (h.JIDs), podUid) {
					// The JID disappeared, so the container is dead, probably from a POD delete request. Trying to get the latest log one last time.
					// Because the moment we found this, there might be some more logs to read.
					isContainerDead = true
					log.G(h.Ctx).Info(sessionContextMessage, "Container is found dead thanks to missing JID, reading last logs...")
				} else if _, err := os.Stat(containerStatusPath); errors.Is(err, os.ErrNotExist) {
					// The status file of the container does not exist, so the container is still alive. Continuing to follow logs.
					// Sleep because otherwise it can be a stress to file system to always read it when it has nothing.
					log.G(h.Ctx).Debug(sessionContextMessage, "EOF of container logs, sleeping 4s before retrying...")
					time.Sleep(4 * time.Second)
				} else {
					// The status file exist, so the container is dead. Trying to get the latest log one last time.
					// Because the moment we found the status file, there might be some more logs to read.
					isContainerDead = true
					log.G(h.Ctx).Info(sessionContextMessage, "Container is found dead thanks to status file, reading last logs...")
				}
				continue
			} else {
				// Unreachable code.
			}
		}
	}
	// No error, err = nil
	return nil
}

// GetLogsHandler reads Jobs' output file to return what's logged inside.
// What's returned is based on the provided parameters (Tail/LimitBytes/Timestamps/etc)
func (h *SidecarHandler) GetLogsHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "GetLogsSLURM", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	// For debugging purpose, when we have many kubectl logs, we can differentiate each one.
	sessionContext := GetSessionContext(r)
	sessionContextMessage := GetSessionContextMessage(sessionContext)

	log.G(h.Ctx).Info(sessionContextMessage, "Docker Sidecar: received GetLogs call")
	var req commonIL.LogStruct
	currentTime := time.Now()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.logErrorVerbose(sessionContextMessage+"error during ReadAll() in GetLogsHandler request body", spanCtx, w, err)
		return
	}

	err = json.Unmarshal(bodyBytes, &req)
	if err != nil {
		h.logErrorVerbose(sessionContextMessage+"error during Unmarshal() in GetLogsHandler request body", spanCtx, w, err)
		return
	}

	span.SetAttributes(
		attribute.String("pod.name", req.PodName),
		attribute.String("pod.namespace", req.Namespace),
		attribute.Int("opts.limitbytes", req.Opts.LimitBytes),
		attribute.Int("opts.since", req.Opts.SinceSeconds),
		attribute.Int64("opts.sincetime", req.Opts.SinceTime.UnixMicro()),
		attribute.Int("opts.tail", req.Opts.Tail),
		attribute.Bool("opts.follow", req.Opts.Follow),
		attribute.Bool("opts.previous", req.Opts.Previous),
		attribute.Bool("opts.timestamps", req.Opts.Timestamps),
	)

	path := h.Config.DataRootFolder + req.Namespace + "-" + req.PodUID
	containerOutputPath := path + "/" + req.ContainerName + ".out"
	var output []byte
	if req.Opts.Timestamps {
		h.logErrorVerbose(sessionContextMessage+"unsupported option req.Opts.Timestamps", spanCtx, w, err)
		return
	}
	containerOutput, err := h.ReadLogs(containerOutputPath, span, spanCtx, w, sessionContextMessage)
	if err != nil {
		// Error already handled in waitAndReadLogs
		return
	}
	jobOutput, err := h.ReadLogs(path+"/"+"job.out", span, spanCtx, w, sessionContextMessage)
	if err != nil {
		// Error already handled in waitAndReadLogs
		return
	}

	output = append(output, jobOutput...)
	output = append(output, containerOutput...)

	var returnedLogs string

	if req.Opts.Tail != 0 {
		var lastLines []string

		splittedLines := strings.Split(string(output), "\n")

		if req.Opts.Tail > len(splittedLines) {
			lastLines = splittedLines
		} else {
			lastLines = splittedLines[len(splittedLines)-req.Opts.Tail-1:]
		}

		for _, line := range lastLines {
			returnedLogs += line + "\n"
		}
	} else if req.Opts.LimitBytes != 0 {
		var lastBytes []byte
		if req.Opts.LimitBytes > len(output) {
			lastBytes = output
		} else {
			lastBytes = output[len(output)-req.Opts.LimitBytes-1:]
		}

		returnedLogs = string(lastBytes)
	} else {
		returnedLogs = string(output)
	}

	if req.Opts.Timestamps && (req.Opts.SinceSeconds != 0 || !req.Opts.SinceTime.IsZero()) {
		temp := returnedLogs
		returnedLogs = ""
		splittedLogs := strings.Split(temp, "\n")
		timestampFormat := "2006-01-02T15:04:05.999999999Z"

		for _, Log := range splittedLogs {
			part := strings.SplitN(Log, " ", 2)
			timestampString := part[0]
			timestamp, err := time.Parse(timestampFormat, timestampString)
			if err != nil {
				continue
			}
			if req.Opts.SinceSeconds != 0 {
				if currentTime.Sub(timestamp).Seconds() > float64(req.Opts.SinceSeconds) {
					returnedLogs += Log + "\n"
				}
			} else {
				if timestamp.Sub(req.Opts.SinceTime).Seconds() >= 0 {
					returnedLogs += Log + "\n"
				}
			}
		}
	}

	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(http.StatusOK))

	//w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Content-Type", "text/plain")

	log.G(h.Ctx).Info(sessionContextMessage, "writing response headers and OK status")
	w.WriteHeader(http.StatusOK)

	log.G(h.Ctx).Info(sessionContextMessage, "writing response body len: ", len(returnedLogs))
	n, err := w.Write([]byte(returnedLogs))
	log.G(h.Ctx).Info(sessionContextMessage, "written response body len: ", n)
	if err != nil {
		h.logErrorVerbose(sessionContextMessage+"error during Write() in GetLogsHandler, could write bytes: "+strconv.Itoa(n), spanCtx, w, err)
		return
	}

	// Flush or else, it could be lost in the pipe.
	if f, ok := w.(http.Flusher); ok {
		log.G(h.Ctx).Debug(sessionContextMessage, "flushing after wrote response body bytes: "+strconv.Itoa(n))
		f.Flush()
	} else {
		log.G(h.Ctx).Error(sessionContextMessage, "wrote response body but could not flush because server does not support Flusher.")
		return
	}

	if req.Opts.Follow {
		err := h.GetLogsFollowMode(spanCtx, req.PodUID, w, r, path, req, containerOutputPath, containerOutput, sessionContext)
		if err != nil {
			h.logErrorVerbose(sessionContextMessage+"follow mode error", spanCtx, w, err)
		}
	}
}

// Goal: read the file if it exist. If not, return empty.
// Important to wait because if we don't wait and return empty array, it will generates a JSON unmarshall error in InterLink VK.
// Fail for any error not related to file not existing (eg: permission error will raise an error).
// Already handle error.
func (h *SidecarHandler) ReadLogs(logsPath string, span trace.Span, ctx context.Context, w http.ResponseWriter, sessionContextMessage string) ([]byte, error) {
	var output []byte
	var err error
	log.G(h.Ctx).Info(sessionContextMessage, "reading file ", logsPath)
	output, err = os.ReadFile(logsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.G(h.Ctx).Info(sessionContextMessage, "file ", logsPath, " not found.")
			output = make([]byte, 0)
		} else {
			span.AddEvent("Error retrieving logs")
			h.logErrorVerbose(sessionContextMessage+"error during ReadFile() of readLogs() in GetLogsHandler of file "+logsPath, ctx, w, err)
			return nil, err
		}
	}
	return output, nil
}
