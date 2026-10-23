package relief

import (
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/spec"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func cfg() spec.Lever {
	return spec.Lever{Enter: 0.25, Exit: 0.10, EnterFor: time.Minute, ExitFor: 3 * time.Minute}
}

// feed observes a fresh reading every 5s from start for d, inclusive of both
// ends, and returns the phase after the last one.
func feed(tr *Trigger, start time.Duration, d time.Duration, p float64) Phase {
	var ph Phase
	for at := start; at <= start+d; at += 5 * time.Second {
		ph = tr.Observe(t0.Add(at), p, true)
	}
	return ph
}

func relieving(t *testing.T) *Trigger {
	t.Helper()
	tr := NewTrigger(cfg())
	if feed(tr, 0, time.Minute, 0.3) != Relieving {
		t.Fatal("setup: not relieving")
	}
	return tr
}

func TestPressureMustLastToEnter(t *testing.T) {
	tr := NewTrigger(cfg())
	if got := feed(tr, 0, 59*time.Second, 0.3); got != Calm {
		t.Fatalf("after 59s: %s", got)
	}
	if got := tr.Observe(t0.Add(60*time.Second), 0.3, true); got != Relieving {
		t.Fatalf("at 60s: %s", got)
	}
}

func TestABriefDipDoesNotEnter(t *testing.T) {
	tr := NewTrigger(cfg())
	feed(tr, 0, 30*time.Second, 0.3)
	tr.Observe(t0.Add(35*time.Second), 0.2, true)
	if got := feed(tr, 40*time.Second, 45*time.Second, 0.3); got != Calm {
		t.Fatalf("got %s", got)
	}
}

func TestCalmStaysCalmUnderLowPressure(t *testing.T) {
	tr := NewTrigger(cfg())
	if got := feed(tr, 0, time.Hour, 0.01); got != Calm {
		t.Fatalf("got %s", got)
	}
}

func TestItLeavesOnlyBelowTheLowerBand(t *testing.T) {
	tr := relieving(t)
	at := 2 * time.Minute
	if got := feed(tr, at, 10*time.Minute, 0.15); got != Relieving {
		t.Fatalf("between bands: %s", got)
	}
	at += 11 * time.Minute
	if got := feed(tr, at, 3*time.Minute, 0.05); got != Releasing {
		t.Fatalf("below exit: %s", got)
	}
}

func releasing(t *testing.T) (*Trigger, time.Duration) {
	t.Helper()
	tr := relieving(t)
	at := 2 * time.Minute
	if feed(tr, at, 3*time.Minute, 0.05) != Releasing {
		t.Fatal("setup: not releasing")
	}
	return tr, at + 4*time.Minute
}

func TestPressureReturningStopsRelease(t *testing.T) {
	tr, at := releasing(t)
	if got := feed(tr, at, time.Minute, 0.3); got != Relieving {
		t.Fatalf("got %s", got)
	}
}

func TestReleasingDoesNotReturnToCalmByItself(t *testing.T) {
	tr, at := releasing(t)
	if got := feed(tr, at, time.Hour, 0.0); got != Releasing {
		t.Fatalf("got %s", got)
	}
}

func TestReleasedMovesReleasingToCalmOnly(t *testing.T) {
	tr := relieving(t)
	tr.Released()
	if tr.Phase() != Relieving {
		t.Fatalf("Released changed %s", tr.Phase())
	}
	tr, _ = releasing(t)
	tr.Released()
	if tr.Phase() != Calm {
		t.Fatalf("got %s", tr.Phase())
	}
}

func TestStaleReadingsHoldThePhase(t *testing.T) {
	tr := relieving(t)
	at := 2 * time.Minute
	for ; at <= 12*time.Minute; at += 5 * time.Second {
		if got := tr.Observe(t0.Add(at), 0, false); got != Relieving {
			t.Fatalf("got %s", got)
		}
	}
}

func TestStaleReadingBreaksARun(t *testing.T) {
	tr := NewTrigger(cfg())
	tr.Observe(t0, 0.3, true)
	if got := tr.Observe(t0.Add(10*time.Minute), 0, false); got != Calm {
		t.Fatalf("stale: %s", got)
	}
	if got := tr.Observe(t0.Add(11*time.Minute), 0.3, true); got != Calm {
		t.Fatalf("one fresh reading after the outage: %s", got)
	}
	if got := feed(tr, 11*time.Minute+5*time.Second, time.Minute, 0.3); got != Relieving {
		t.Fatalf("a minute of fresh readings: %s", got)
	}
}

func TestReadingsAtTheThresholdDoNotCount(t *testing.T) {
	tr := NewTrigger(cfg())
	if got := feed(tr, 0, time.Hour, 0.25); got != Calm {
		t.Fatalf("at Enter: %s", got)
	}
	tr = relieving(t)
	if got := feed(tr, 2*time.Minute, time.Hour, 0.10); got != Relieving {
		t.Fatalf("at Exit: %s", got)
	}
}

func TestAReadingBetweenBandsRestartsTheExitClock(t *testing.T) {
	tr := relieving(t)
	at := 2 * time.Minute
	feed(tr, at, 2*time.Minute, 0.05)
	tr.Observe(t0.Add(at+2*time.Minute+5*time.Second), 0.15, true)
	at += 2*time.Minute + 10*time.Second
	if got := feed(tr, at, 2*time.Minute+50*time.Second, 0.05); got != Relieving {
		t.Fatalf("before a full ExitFor: %s", got)
	}
	if got := tr.Observe(t0.Add(at+3*time.Minute), 0.05, true); got != Releasing {
		t.Fatalf("after a full ExitFor: %s", got)
	}
}

func TestACoolReadingBreaksARunInReleasing(t *testing.T) {
	tr, at := releasing(t)
	feed(tr, at, 50*time.Second, 0.3)
	tr.Observe(t0.Add(at+55*time.Second), 0.25, true)
	if got := feed(tr, at+60*time.Second, 50*time.Second, 0.3); got != Releasing {
		t.Fatalf("got %s", got)
	}
	if got := tr.Observe(t0.Add(at+120*time.Second), 0.3, true); got != Relieving {
		t.Fatalf("got %s", got)
	}
}
