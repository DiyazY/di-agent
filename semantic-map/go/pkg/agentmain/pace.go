package agentmain

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// The pace is the agent's attention level. After paceCalmFor of calm the paced
// loops run at paceSlowFactor × their cadence; any event returns them to base at once.
const (
	paceBase       = statemap.PaceBase
	paceSlow       = statemap.PaceSlow
	paceSlowFactor = 4
	paceCalmFor    = 10 * time.Minute
	paceTick       = time.Minute
)

// PaceInputs is what the pace reads each tick. Each is read outside the pace lock and
// may be nil, which reads as calm (no levers, nothing pending, nothing due, nothing admitted).
type PaceInputs struct {
	LeverPhases    func() []string  // relief.Status().Phase per lever; every one "calm" is calm — vacuously so with no levers
	PendingAccepts func() int       // len(tracker.PendingList())
	ReviewDue      func() int       // autonomy Status.Due; 0 with no reviewer
	LastAdmitted   func() time.Time // Map.LastAdmitted
}

// Pace decides the attention level and records each change on the map.
type Pace struct {
	mu       sync.Mutex
	m        *statemap.Map
	in       PaceInputs
	now      func() time.Time
	logf     func(string, ...any)
	pace     string // mirrors m.Pace(); the map is the record, this is the policy's copy
	since    time.Time
	calmFrom time.Time // when the current calm began; zero while any condition fails
	wakes    uint64    // incremented by Wake; a tick whose read straddled a wake discards it
	pinned   string    // non-empty: never slow, and why
}

// NewPace builds the pace for m, wakes it on every admission, and takes one tick so
// a calm present at construction is counted from construction.
func NewPace(m *statemap.Map, in PaceInputs, now func() time.Time, logf func(string, ...any)) *Pace {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = log.Printf
	}
	p := &Pace{m: m, in: in, now: now, logf: logf, pace: m.Pace(), since: m.PaceSince()}
	m.SetAdmitHook(func(id string) { p.Wake("property admitted: " + id) })
	p.Tick()
	return p
}

// Pin keeps the pace at base for good; why is logged once.
func (p *Pace) Pin(why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pinned != "" {
		return
	}
	p.pinned = why
	p.logf("pace: never slow: %s", why)
}

// Current is the pace and when it last changed.
func (p *Pace) Current() (pace string, since time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pace, p.since
}

// Interval is base at base pace (or with no pace), paceSlowFactor × base when slow.
func (p *Pace) Interval(base time.Duration) time.Duration {
	if p == nil {
		return base
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pace == paceSlow {
		return base * paceSlowFactor
	}
	return base
}

// Wake restarts the calm count at now and, if slow, returns to base at once.
func (p *Pace) Wake(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calmFrom = p.now()
	p.wakes++
	if p.pace == paceSlow {
		p.set(paceBase, reason)
	}
}

// Tick checks the inputs once at now().
func (p *Pace) Tick() {
	p.mu.Lock()
	w := p.wakes
	p.mu.Unlock()

	// Read outside the lock: an input may itself wake the pace.
	var phases []string
	if p.in.LeverPhases != nil {
		phases = p.in.LeverPhases()
	}
	pending, due := 0, 0
	if p.in.PendingAccepts != nil {
		pending = p.in.PendingAccepts()
	}
	if p.in.ReviewDue != nil {
		due = p.in.ReviewDue()
	}
	var last time.Time
	if p.in.LastAdmitted != nil {
		last = p.in.LastAdmitted()
	}
	now := p.now()

	failing := ""
	for _, ph := range phases {
		if ph != "calm" {
			failing = "relief " + ph
			break
		}
	}
	if failing == "" && pending != 0 {
		failing = "accept pending"
	}
	if failing == "" && due != 0 {
		failing = "review due"
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wakes != w {
		return // a wake already handled this tick's window
	}
	if failing != "" {
		p.calmFrom = time.Time{}
		if p.pace == paceSlow {
			p.set(paceBase, failing)
		}
		return
	}
	if p.calmFrom.IsZero() || now.Before(p.calmFrom) {
		p.calmFrom = now // counted from the tick that observes it; a backward step restarts it
	}
	if last.After(p.calmFrom) {
		p.calmFrom = last // calm began no earlier than the last admission
	}
	if p.pace == paceBase && p.pinned == "" && now.Sub(p.calmFrom) >= paceCalmFor {
		p.set(paceSlow, "calm for "+paceCalmFor.String())
	}
}

// set changes the pace on the map and mirrors it. Caller holds p.mu.
func (p *Pace) set(to, reason string) {
	from := p.pace
	if p.m.SetPace(to, reason) {
		p.pace, p.since = to, p.now()
		p.logf("pace: %s → %s: %s", from, to, reason)
	}
}

// Run ticks every paceTick of wall clock (the pace itself is never paced) until ctx ends.
func (p *Pace) Run(ctx context.Context) {
	t := time.NewTicker(paceTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick()
		}
	}
}

// wait sleeps p.Interval(base) or until ctx is done; false means stop. nil-safe.
func (p *Pace) wait(ctx context.Context, base time.Duration) bool {
	t := time.NewTimer(p.Interval(base))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// paceTelemetryPath is the pushed-sample route: telemetry, not an attention event. A
// sample that admits a new property still wakes the pace through the admit hook.
const paceTelemetryPath = "/ingest-sample"

// Wakeful wakes the pace on every request that may change something: any method
// other than GET, HEAD and OPTIONS, except a pushed sample.
func (p *Pace) Wakeful(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet, r.Method == http.MethodHead, r.Method == http.MethodOptions:
		case r.URL.Path == paceTelemetryPath:
		default:
			p.Wake("request:" + r.Method + " " + r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// paceAllowed reports whether slowing collection by paceSlowFactor keeps every
// property inside stale_after; sources.interval 0 (collection off) always allows it.
func paceAllowed(sourcesInterval, staleAfter time.Duration) (ok bool, why string) {
	if sourcesInterval > 0 && sourcesInterval*paceSlowFactor >= staleAfter {
		return false, fmt.Sprintf("sources.interval %s × %d ≥ map.stale_after %s: a slow collection would let every property go stale",
			sourcesInterval, paceSlowFactor, staleAfter)
	}
	return true, ""
}
