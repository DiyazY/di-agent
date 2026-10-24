// Package relief decides when a node is under pressure and when it is not, and
// acts on the answer through one lever per action. This file is the decision
// alone.
package relief

import (
	"time"

	"github.com/DiyazY/di-agent/pkg/spec"
)

// Phase is where the node stands: calm, under pressure with relief in force,
// or cool enough that relief is being undone.
type Phase string

const (
	Calm      Phase = "calm"
	Relieving Phase = "relieving"
	Releasing Phase = "releasing"
)

// Trigger turns readings of one kind of node pressure into a phase. A phase changes
// only after pressure has stayed on one side of a threshold for a set time,
// so a spike does not start relief and a lull does not end it. A Trigger is
// not safe for concurrent use; the loop that owns it calls it from one
// goroutine.
type Trigger struct {
	cfg     spec.Lever
	phase   Phase
	runFrom time.Time
	running bool
}

// NewTrigger returns a Trigger in Calm.
func NewTrigger(c spec.Lever) *Trigger {
	return &Trigger{cfg: c, phase: Calm}
}

// Phase is the current phase.
func (t *Trigger) Phase() Phase { return t.phase }

// Observe takes a reading at now and returns the phase after it. A run is
// consecutive fresh readings on the side of a threshold that would change the
// phase; it begins with the first of them and completes once its age reaches
// the required time. Any reading off that side breaks the run. Calm and
// Releasing both enter Relieving on pressure above Enter for EnterFor;
// Relieving enters Releasing on pressure below Exit for ExitFor. A reading
// with fresh false (the property is stale or absent) holds the phase but
// breaks any run in progress, so a run is only ever consecutive fresh readings
// and one reading after an outage cannot complete a run begun before it.
func (t *Trigger) Observe(now time.Time, pressure float64, fresh bool) Phase {
	if !fresh {
		t.running = false
		return t.phase
	}
	var (
		on   bool
		need time.Duration
		next Phase
	)
	switch t.phase {
	case Relieving:
		on, need, next = pressure < t.cfg.Exit, t.cfg.ExitFor, Releasing
	default:
		on, need, next = pressure > t.cfg.Enter, t.cfg.EnterFor, Relieving
	}
	if !on {
		t.running = false
		return t.phase
	}
	if !t.running {
		t.running, t.runFrom = true, now
	}
	if now.Sub(t.runFrom) >= need {
		t.phase, t.running = next, false
	}
	return t.phase
}

// Released records that nothing is left to revert, which is the only way out
// of Releasing. In any other phase it does nothing.
func (t *Trigger) Released() {
	if t.phase == Releasing {
		t.phase, t.running = Calm, false
	}
}
