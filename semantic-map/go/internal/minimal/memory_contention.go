package minimal

import (
	"math"
	"sync/atomic"
)

// Requests is the pods' memory requests in bytes, by pod UID, as the runtime last listed
// them. The collector reads it every collect; whoever lists the workloads stores it. It is
// safe for concurrent Store and Load, and the zero value holds nothing yet.
type Requests struct {
	m atomic.Pointer[map[string]float64]
}

// Store replaces the requests with a copy of m, so a later write to m changes nothing here.
func (r *Requests) Store(m map[string]float64) {
	cp := make(map[string]float64, len(m))
	for k, v := range m {
		cp[k] = v
	}
	r.m.Store(&cp)
}

// Load is the requests last stored, and false until the first Store: before it nothing is
// known about any pod's request, which is not the same as every pod having none. The map
// is shared and must not be written.
func (r *Requests) Load() (map[string]float64, bool) {
	p := r.m.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
}

// podMem is what memoryContention needs of one pod promised memory (not BestEffort) with a
// memory stall reading this window: its memory.current in bytes (NaN when unread), its PSI
// memory and io "some" stall shares, its memory_refault_ratio (NaN when unread), and
// whether it was limiting itself — its memory.events high or max rose this window.
type podMem struct {
	uid                 string
	current, psi, ioPSI float64
	refault             float64
	selfLimited         bool
}

// refaultGate is the memory_refault_ratio (1/s) from which a pod's io stall counts as
// memory contention: its whole page cache read back within 100 s. A pod that reads its
// files with read(2) and is pushed out of its cache stalls on I/O, not on memory — on an
// MGLRU kernel such a refault is memory PSI only for pages in the highest access tier —
// and io stall without refaults is a busy disk, which no memory cap relieves.
const refaultGate = 0.01

// memoryContention is the node's memory contention: the worst memory stall of a pod that is
// waiting on memory other pods hold rather than on its own limit. A pod's stall is its memory
// PSI, or its io PSI when larger and the pod refaults at refaultGate or faster. A pod pressing against its
// own memory.high or memory.max stalls on its own reclaim, and relieving the node would not
// help it; a pod using more than it requested was promised only its request, so its stall
// is the share it took past it. With requests unknown (haveReq false: no runtime, or none
// listed yet) the request condition is dropped; with them known, a pod with no request, or
// whose memory.current is unread, is not counted. No counted pod — none given, or every one
// excluded — is 0, a reading in its own right rather than the absence of one.
func memoryContention(pods []podMem, req map[string]float64, haveReq bool) float64 {
	worst := 0.0
	for _, p := range pods {
		if p.selfLimited {
			continue
		}
		if haveReq {
			r, ok := req[p.uid]
			if !ok || !(p.current <= r) {
				continue
			}
		}
		stall := p.psi
		if p.refault >= refaultGate {
			stall = math.Max(stall, p.ioPSI)
		}
		worst = math.Max(worst, stall)
	}
	return worst
}
