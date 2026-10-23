package slurm

import (
	"sync"
	"time"

	commonIL "github.com/interlink-hq/interlink/pkg/interlink"
	v1 "k8s.io/api/core/v1"
)

// statusCacheTTL is how long a pod's status is answered from the cache before
// squeue is asked again.
const statusCacheTTL = 10 * time.Second

type cachedPodStatus struct {
	status commonIL.PodStatus
	at     time.Time
}

// The status cache is kept per pod: the virtual kubelet asks about one pod per
// request, so a single cached answer would be returned for the wrong pod.
var (
	statusCacheMu sync.Mutex
	statusCache   = map[string]cachedPodStatus{}
)

// cachedStatuses splits the requested pods into those with a status younger than
// statusCacheTTL, returned by UID, and those squeue must be asked about.
func cachedStatuses(req []*v1.Pod, now time.Time) (map[string]commonIL.PodStatus, []*v1.Pod) {
	statusCacheMu.Lock()
	defer statusCacheMu.Unlock()

	cached := make(map[string]commonIL.PodStatus, len(req))
	var stale []*v1.Pod
	for _, pod := range req {
		entry, ok := statusCache[string(pod.UID)]
		if ok && now.Sub(entry.at) < statusCacheTTL {
			cached[string(pod.UID)] = entry.status
			continue
		}
		stale = append(stale, pod)
	}
	return cached, stale
}

// storeStatuses caches freshly read statuses and forgets pods nobody asked about
// for a while, such as deleted ones.
func storeStatuses(statuses []commonIL.PodStatus, now time.Time) {
	statusCacheMu.Lock()
	defer statusCacheMu.Unlock()

	for _, s := range statuses {
		statusCache[s.PodUID] = cachedPodStatus{status: s, at: now}
	}
	for uid, entry := range statusCache {
		if now.Sub(entry.at) > 6*statusCacheTTL {
			delete(statusCache, uid)
		}
	}
}

// inRequestOrder returns the status of every requested pod that has one, cached or
// fresh, in the order of the request.
func inRequestOrder(req []*v1.Pod, cached map[string]commonIL.PodStatus, fresh []commonIL.PodStatus) []commonIL.PodStatus {
	byUID := make(map[string]commonIL.PodStatus, len(cached)+len(fresh))
	for uid, s := range cached {
		byUID[uid] = s
	}
	for _, s := range fresh {
		byUID[s.PodUID] = s
	}
	out := make([]commonIL.PodStatus, 0, len(req))
	for _, pod := range req {
		if s, ok := byUID[string(pod.UID)]; ok {
			out = append(out, s)
		}
	}
	return out
}

func resetStatusCache() {
	statusCacheMu.Lock()
	defer statusCacheMu.Unlock()
	statusCache = map[string]cachedPodStatus{}
}
