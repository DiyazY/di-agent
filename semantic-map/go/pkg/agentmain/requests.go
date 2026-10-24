package agentmain

import (
	"context"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// requestsEvery is how often the pods' memory requests are listed again: a pod
// started since is counted against its request from the next refresh.
const requestsEvery = 30 * time.Second

// requestsRefresher keeps the cgroup collector's memory requests to what the
// runtime lists: each pod's memory request, by UID. A listing that fails keeps
// the last requests, and the failure is logged once until a listing succeeds.
type requestsRefresher struct {
	rt      runtime.Runtime
	req     *minimal.Requests
	logf    func(string, ...any)
	failing bool
}

// refresh lists the workloads once and stores their requests.
func (r *requestsRefresher) refresh(ctx context.Context) {
	ws, err := r.rt.Workloads(ctx)
	if err != nil {
		if !r.failing {
			r.logf("memory requests: listing workloads: %v; memory contention counts pods against the requests last listed", err)
		}
		r.failing = true
		return
	}
	if r.failing {
		r.logf("memory requests: listing workloads again")
	}
	r.failing = false
	m := map[string]float64{}
	for _, w := range ws {
		if !strings.HasPrefix(w.Subject, "pod:") || w.UID == "" {
			continue
		}
		if req := w.Resources["memory"].Request; req != nil {
			m[w.UID] = *req
		}
	}
	r.req.Store(m)
}

// startRequestsLoop refreshes req from rt once now and every requestsEvery
// until ctx ends, and reports whether it started. On the dry run it does not:
// no pod there has a request, and with none stored the collector drops the
// condition that a pod be within its request rather than count no pod.
func startRequestsLoop(ctx context.Context, kind string, rt runtime.Runtime, req *minimal.Requests, logf func(string, ...any)) bool {
	if kind == "dry-run" {
		return false
	}
	r := &requestsRefresher{rt: rt, req: req, logf: logf}
	go func() {
		r.refresh(ctx)
		t := time.NewTicker(requestsEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.refresh(ctx)
			}
		}
	}()
	return true
}
