package slurm

// Pod group create.
//
// /createPodGroup takes the pods of one pod group in one call and submits them as one
// `sbatch --nodes=N` job, the gang job of gang.go, without buffering the members
// until the last one arrives, as /create does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	"k8s.io/apimachinery/pkg/types"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// CapabilityPodGroupCreate is listed in the ping answer when the plugin takes the
// pods of a pod group in one /createPodGroup call.
const CapabilityPodGroupCreate = "pod-group-create"

// PodGroupCreateRequest is the body of /createPodGroup: the pods of one pod
// group in rank order, each as the interLink API server prepares a pod for
// /create; rank 0 is the head.
type PodGroupCreateRequest struct {
	// GroupUID names the group, and the job: the Workload UID.
	GroupUID string                      `json:"groupUID"`
	Pods     []commonIL.RetrievedPodData `json:"pods"`
}

// CreatePodGroupHandler submits the pods of a group as one job. It answers one
// CreateStruct per pod, in the order of the request, all with the group's job
// ID. It fails with 400 for a group it or Slurm refuses, 503 when trying again
// later can work, and otherwise with the status codes of /create.
func (h *SidecarHandler) CreatePodGroupHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	spanCtx, span := tracer.Start(h.Ctx, "CreatePodGroup", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer commonIL.SetDurationSpan(start, span)

	log.G(h.Ctx).Info("Slurm Sidecar: received CreatePodGroup call")
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.handleError(spanCtx, w, http.StatusInternalServerError, err)
		return
	}
	var req PodGroupCreateRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		h.handleError(spanCtx, w, http.StatusBadRequest, err)
		return
	}

	created, cerr := h.createPodGroup(spanCtx, req)
	if cerr != nil {
		code := cerr.code
		if code == 0 {
			code = http.StatusInternalServerError
		}
		h.handleError(spanCtx, w, code, cerr.err)
		return
	}
	out, err := json.Marshal(created)
	if err != nil {
		h.handleError(spanCtx, w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(http.StatusOK))
	w.Write(out)
}

// createPodGroup renders every pod of the group as /create does and submits them
// as one gang job (submitGang). The whole call holds h.GangMu, so that a retried
// call never renders into the directories of one in flight. A retried call whose
// pods already share a job gets that job again.
func (h *SidecarHandler) createPodGroup(ctx context.Context, req PodGroupCreateRequest) ([]CreateStruct, *createError) {
	span := trace.SpanFromContext(ctx)
	refuse := func(code int, format string, args ...any) ([]CreateStruct, *createError) {
		return nil, &createError{code: code, err: fmt.Errorf(format, args...)}
	}
	if !h.Config.GangSchedulingEnabled {
		return refuse(http.StatusBadRequest, "this plugin does not run pod groups: GangSchedulingEnabled is off")
	}
	name := strings.TrimSpace(req.GroupUID)
	if name == "" {
		return refuse(http.StatusBadRequest, "groupUID is empty")
	}
	var pods []commonIL.RetrievedPodData
	seen := map[types.UID]bool{}
	for _, data := range req.Pods {
		if data.Pod.UID == "" || seen[data.Pod.UID] {
			return refuse(http.StatusBadRequest, "pod %s has no UID, or the UID of another pod", data.Pod.Name)
		}
		if group := gangNameFromMeta(data.Pod.ObjectMeta); group != "" && group != name {
			return refuse(http.StatusBadRequest, "pod %s belongs to group %s, not %s", data.Pod.Name, group, name)
		}
		seen[data.Pod.UID] = true
		pods = append(pods, data)
	}
	if len(pods) == 0 {
		return refuse(http.StatusBadRequest, "group %s has no pods", name)
	}
	span.SetAttributes(attribute.String("group.uid", name), attribute.Int("group.pods", len(pods)))

	h.GangMu.Lock()
	defer h.GangMu.Unlock()
	if h.GangTable == nil {
		h.GangTable = make(map[string]*GangEntry)
	}
	h.sweepGangsLocked(ctx)

	if jid := h.sharedJobLocked(pods); jid != "" {
		log.G(ctx).Infof("Group '%s': already submitted as SLURM job %s", name, jid)
		return groupCreated(pods, jid), nil
	}
	// /create keys the same group by its pods' namespace too (gangKey).
	keys := []string{name}
	for _, data := range pods {
		keys = append(keys, gangKey(data.Pod.Namespace, name))
	}
	for _, key := range keys {
		entry, ok := h.GangTable[key]
		if !ok {
			continue
		}
		if entry.Buffering {
			return refuse(http.StatusServiceUnavailable, "group %s is being created pod by pod (%d/%d)", name, len(entry.Members), entry.Size)
		}
		if !entry.Submitted {
			// sbatch ran, but its job ID was not recorded: the job may run. The
			// entry goes with the /delete of its pods, the last of which cancels
			// the job if sbatch printed its ID.
			return refuse(http.StatusServiceUnavailable, "group %s: sbatch ran but its job was not recorded (%d of %d pods not deleted yet)", name, len(entry.Members), entry.Size)
		}
		if !gangJobIsGone(ctx, h.Config, entry.JID) {
			return refuse(http.StatusServiceUnavailable, "group %s still runs as job %s", name, entry.JID)
		}
		delete(h.GangTable, key)
	}

	entry := &GangEntry{
		Name:    name,
		Size:    len(pods),
		Members: make(map[string]*BufferedMember, len(pods)),
	}
	ordered := make([]*BufferedMember, 0, len(pods))
	var rendered []*renderedPod
	removeRendered := func() {
		for _, p := range rendered {
			os.RemoveAll(p.filesPath)
			if p.workDir != p.filesPath {
				os.RemoveAll(p.workDir)
			}
		}
	}
	for i, data := range pods {
		p, cerr := h.renderPod(ctx, data)
		if cerr != nil {
			removeRendered()
			return nil, cerr
		}
		rendered = append(rendered, p)
		m := gangMemberFromCreate(data.Pod, p.filesPath, p.workDir, p.runtimeCommands, p.resourceLimits, p.isDefaultCPU, p.isDefaultRAM, p.flavor)
		m.Rank, m.Role = i, GangRoleWorker
		if i == 0 {
			m.Role = GangRoleHead
		}
		entry.Members[m.PodUID] = m
		ordered = append(ordered, m)
	}

	jid, err := h.submitGang(ctx, entry, ordered)
	if err != nil {
		span.AddEvent("Failed to submit the group SLURM Job")
		if errors.Is(err, errGangSubmitted) {
			// The job may run: the entry stays until the /delete of its pods, the
			// last of which cancels the job if sbatch printed its ID.
			h.GangTable[name] = entry
		} else {
			removeRendered()
		}
		return nil, submitFailure(err)
	}
	h.GangTable[name] = entry
	span.AddEvent("Group SLURM Job successfully submitted with ID " + jid)
	return groupCreated(pods, jid), nil
}

// sharedJobLocked returns the job every pod already has as a gang member, if they
// share one. The caller MUST hold h.GangMu.
func (h *SidecarHandler) sharedJobLocked(pods []commonIL.RetrievedPodData) string {
	if h.JIDs == nil {
		return ""
	}
	jid := ""
	for _, data := range pods {
		j, ok := (*h.JIDs)[string(data.Pod.UID)]
		if !ok || !j.Gang || j.JID == "" || (jid != "" && j.JID != jid) {
			return ""
		}
		jid = j.JID
	}
	return jid
}

// groupCreated is the answer to /createPodGroup: every pod with the group's job.
func groupCreated(pods []commonIL.RetrievedPodData, jid string) []CreateStruct {
	created := make([]CreateStruct, len(pods))
	for i, data := range pods {
		created[i] = CreateStruct{PodUID: string(data.Pod.UID), PodJID: jid}
	}
	return created
}

// sbatchLaterRe matches sbatch refusals that end once other jobs end or the
// controller answers again: limits on the number of submitted jobs, and an
// unreachable or busy controller. A refused account or an invalid script stays
// permanent.
var sbatchLaterRe = regexp.MustCompile(`(QOS|Assoc)MaxSubmitJob[A-Za-z]*|MaxSubmitJobs?Limit|Unable to contact slurm controller|Socket timed out|Slurm temporarily unable to accept job`)

// submitFailure is the answer to a failed sbatch. ssh failing (exit code 255) and
// the refusals of sbatchLaterRe are 503; any other refusal by sbatch, such as an
// invalid account or partition, is 400 with sbatch's text. Not running sbatch at
// all stays 504, as in /create.
func submitFailure(err error) *createError {
	var se *SbatchError
	if !errors.As(err, &se) {
		return &createError{code: http.StatusGatewayTimeout, err: err}
	}
	if se.ExitCode == 255 {
		return &createError{code: http.StatusServiceUnavailable, err: fmt.Errorf("ssh to the site failed: %s", strings.TrimSpace(se.Stderr))}
	}
	if m := sbatchLaterRe.FindString(se.Stderr); m != "" {
		return &createError{code: http.StatusServiceUnavailable, err: fmt.Errorf("slurm takes no more jobs of this user now (%s): %s", m, strings.TrimSpace(se.Stderr))}
	}
	return &createError{code: http.StatusBadRequest, err: se}
}
