package collectors

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

const testNode = "n1"

// fakeCollector is a scriptable member. samples is read under mu so a test can
// change what the member emits between ticks; release unblocks a member held by
// delay so its abandoned goroutine ends before the test does.
type fakeCollector struct {
	mu      sync.Mutex
	id      string
	samples []*types.MetricSample
	err     error
	delay   time.Duration
	panics  bool
	avail   []types.MetricType
	release chan struct{}
	calls   atomic.Int64
}

func (f *fakeCollector) Collect() ([]*types.MetricSample, error) {
	f.calls.Add(1)
	f.mu.Lock()
	samples, err, delay, panics, release := f.samples, f.err, f.delay, f.panics, f.release
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-release:
		}
	}
	if panics {
		panic("boom")
	}
	return samples, err
}

func (f *fakeCollector) SourceID() string { return f.id }

func (f *fakeCollector) AvailableMetrics() []types.MetricType {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.avail
}

func (f *fakeCollector) set(samples []*types.MetricSample, err error) {
	f.mu.Lock()
	f.samples, f.err = samples, err
	f.mu.Unlock()
}

// fakeClock is advanced by hand; backoff deadlines are computed from it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func sample(mt string, unit string) *types.MetricSample {
	return &types.MetricSample{NodeID: testNode, MetricType: types.MetricType(mt), Value: 0.5, Unit: unit}
}

func member(name string, c *fakeCollector) Member {
	return Member{Name: name, Kind: "type", Type: "fake", C: c, Timeout: time.Second}
}

func newTestSupervisor(t *testing.T, members ...Member) (*Supervisor, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewSupervisor(Env{NodeID: testNode, Logf: t.Logf}, members)
	s.now = clk.Now
	return s, clk
}

func statusOf(t *testing.T, s *Supervisor, name string) MemberStatus {
	t.Helper()
	for _, st := range s.Status() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no status for member %q", name)
	return MemberStatus{}
}

func TestSupervisorIsolatesErrorAndPanic(t *testing.T) {
	a := &fakeCollector{id: "a", err: errors.New("source down")}
	b := &fakeCollector{id: "b", panics: true}
	c := &fakeCollector{id: "c", samples: []*types.MetricSample{sample("x", "share"), sample("y", "share")}}
	s, _ := newTestSupervisor(t, member("a", a), member("b", b), member("c", c))

	out, err := s.Collect()
	if err != nil {
		t.Fatalf("Collect error = %v, want nil", err)
	}
	if len(out) != 2 {
		t.Fatalf("Collect returned %d samples, want 2", len(out))
	}
	for _, name := range []string{"a", "b"} {
		st := statusOf(t, s, name)
		if st.State != "backoff" || st.LastError == "" {
			t.Errorf("%s: state=%q last_error=%q, want backoff with an error", name, st.State, st.LastError)
		}
	}
	if st := statusOf(t, s, "c"); st.Samples != 2 || st.State != "ok" {
		t.Errorf("c: samples=%d state=%q, want 2 ok", st.Samples, st.State)
	}
}

func TestSupervisorSlowMemberDoesNotDelayTick(t *testing.T) {
	slow := &fakeCollector{id: "slow", delay: 2 * time.Second, release: make(chan struct{})}
	t.Cleanup(func() { close(slow.release) })
	m := member("slow", slow)
	m.Timeout = 100 * time.Millisecond
	s, clk := newTestSupervisor(t, m)

	start := time.Now()
	if _, err := s.Collect(); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el >= time.Second {
		t.Fatalf("Collect took %v, want < 1s", el)
	}
	if st := statusOf(t, s, "slow"); st.State != "stuck" || st.LastError == "" {
		t.Fatalf("after timeout: state=%q last_error=%q, want stuck with the timeout", st.State, st.LastError)
	}

	// Past the backoff the member would be due again, but its first call has not
	// returned, so it must not be called a second time.
	clk.Advance(time.Hour)
	if _, err := s.Collect(); err != nil {
		t.Fatal(err)
	}
	if n := slow.calls.Load(); n != 1 {
		t.Fatalf("member called %d times, want 1 while its first call is in flight", n)
	}
	if st := statusOf(t, s, "slow"); st.State != "stuck" {
		t.Fatalf("in-flight member state=%q, want stuck", st.State)
	}
}

// When an abandoned call finally ends, status stops saying stuck, and an error it
// ended with becomes the member's last error rather than being lost.
func TestSupervisorAbandonedCallEndingIsRecorded(t *testing.T) {
	slow := &fakeCollector{id: "slow", delay: time.Hour, release: make(chan struct{}), err: errors.New("device wedged")}
	m := member("slow", slow)
	m.Timeout = 50 * time.Millisecond
	sink := &logSink{}
	s := NewSupervisor(Env{NodeID: testNode, Logf: sink.logf}, []Member{m})
	s.Collect()
	if st := statusOf(t, s, "slow"); st.State != "stuck" {
		t.Fatalf("state=%q, want stuck", st.State)
	}
	// Outlast the timeout's own 1s backoff before the call ends.
	time.Sleep(1500 * time.Millisecond)
	close(slow.release)
	deadline := time.Now().Add(2 * time.Second)
	for statusOf(t, s, "slow").State == "stuck" {
		if time.Now().After(deadline) {
			t.Fatal("still stuck after the call returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := statusOf(t, s, "slow")
	if st.State != "backoff" || st.LastError != "abandoned call ended: device wedged" {
		t.Fatalf("status = %+v, want backoff with the late error", st)
	}
	// Recorded as a fault now: the backoff runs from the call's end, not from the
	// timeout it has long outlasted.
	if st.BackoffUntilUnix < time.Now().Unix() || st.LastErrorUnix < st.LastSuccessUnix {
		t.Fatalf("status = %+v, want a backoff that has not yet passed", st)
	}
	if sink.count("collector[slow]: abandoned call ended: device wedged") != 1 {
		t.Fatalf("log = %q", sink.lines)
	}
}

// A panic in AvailableMetrics before the first collect is survived and logged.
func TestSupervisorAvailablePanicIsLogged(t *testing.T) {
	sink := &logSink{}
	NewSupervisor(Env{NodeID: testNode, Logf: sink.logf}, []Member{{Name: "p", Kind: "type", C: &panickyAvailable{}}})
	if sink.count("collector[p]: AvailableMetrics panicked before the first collect: no types yet") != 1 {
		t.Fatalf("log = %q", sink.lines)
	}
}

type panickyAvailable struct{ fakeCollector }

func (*panickyAvailable) AvailableMetrics() []types.MetricType { panic("no types yet") }

func TestSupervisorBackoffGrowsAndResets(t *testing.T) {
	f := &fakeCollector{id: "f", err: errors.New("down")}
	s, clk := newTestSupervisor(t, member("f", f))

	for i, want := range []int64{1, 2, 4} {
		if _, err := s.Collect(); err != nil {
			t.Fatal(err)
		}
		st := statusOf(t, s, "f")
		if got := st.BackoffUntilUnix - clk.Now().Unix(); got != want {
			t.Fatalf("fault %d: backoff %ds, want %ds", i+1, got, want)
		}
		clk.Advance(time.Duration(want) * time.Second)
	}

	f.set([]*types.MetricSample{sample("x", "share")}, nil)
	if _, err := s.Collect(); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, s, "f"); st.State != "ok" || st.BackoffUntilUnix != 0 {
		t.Fatalf("after success: state=%q backoff_until=%d", st.State, st.BackoffUntilUnix)
	}

	f.set(nil, errors.New("down again"))
	if _, err := s.Collect(); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s, "f").BackoffUntilUnix - clk.Now().Unix(); got != 1 {
		t.Fatalf("first fault after a success: backoff %ds, want 1s", got)
	}
}

func TestSupervisorBackoffSkipsMemberUntilDue(t *testing.T) {
	f := &fakeCollector{id: "f", err: errors.New("down")}
	s, clk := newTestSupervisor(t, member("f", f))
	s.Collect()
	s.Collect() // still inside the 1 s backoff
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("called %d times inside backoff, want 1", n)
	}
	clk.Advance(time.Second)
	s.Collect()
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("called %d times after backoff, want 2", n)
	}
}

func TestSupervisorBackoffCapsAtFiveMinutes(t *testing.T) {
	f := &fakeCollector{id: "f", err: errors.New("down")}
	s, clk := newTestSupervisor(t, member("f", f))
	var last int64
	for i := 0; i < 80; i++ {
		s.Collect()
		last = statusOf(t, s, "f").BackoffUntilUnix - clk.Now().Unix()
		clk.Advance(time.Duration(last) * time.Second)
	}
	if last != 300 {
		t.Fatalf("backoff after many faults = %ds, want 300s", last)
	}
}

func TestSupervisorDropsViolatingSamplesKeepsBatch(t *testing.T) {
	noUnit := sample("b", "")
	badType := sample("a@b", "share")
	otherNode := sample("c", "share")
	otherNode.NodeID = "other"
	emptyRange := sample("d", "share")
	emptyRange.Range = &[2]float64{1, 1}
	f := &fakeCollector{id: "f", samples: []*types.MetricSample{sample("ok", "share"), noUnit, badType, otherNode, emptyRange}}
	s, _ := newTestSupervisor(t, member("f", f))

	out, _ := s.Collect()
	if len(out) != 1 || out[0].MetricType != "ok" {
		t.Fatalf("got %d samples, want only the valid one", len(out))
	}
	st := statusOf(t, s, "f")
	if st.Violations != 4 || st.Samples != 1 {
		t.Fatalf("violations=%d samples=%d, want 4 and 1", st.Violations, st.Samples)
	}
	if st.State != "ok" {
		t.Fatalf("a batch with violations is not a fault: state=%q", st.State)
	}
}

func TestSupervisorInvalidSubjectIsViolation(t *testing.T) {
	bad := sample("x", "share")
	bad.Subject = "no-kind"
	f := &fakeCollector{id: "f", samples: []*types.MetricSample{bad}}
	s, _ := newTestSupervisor(t, member("f", f))
	if out, _ := s.Collect(); len(out) != 0 {
		t.Fatalf("sample with invalid subject delivered")
	}
	if v := statusOf(t, s, "f").Violations; v != 1 {
		t.Fatalf("violations=%d, want 1", v)
	}
}

// part_of is held to the rule POST /ingest-sample applies: a node-level metric
// type, on a sample with a subject.
func TestSupervisorBadPartOfIsViolation(t *testing.T) {
	unscoped := sample("x", "share")
	unscoped.PartOf = "cpu"
	badName := sample("y", "share")
	badName.Subject = "pod:a"
	badName.PartOf = "cpu util"
	good := sample("z", "share")
	good.Subject = "pod:a"
	good.PartOf = "cpu"
	f := &fakeCollector{id: "f", samples: []*types.MetricSample{unscoped, badName, good}}
	s, _ := newTestSupervisor(t, member("f", f))
	out, _ := s.Collect()
	if len(out) != 1 || out[0].MetricType != "z" {
		t.Fatalf("got %d samples, want only the valid part_of", len(out))
	}
	if v := statusOf(t, s, "f").Violations; v != 2 {
		t.Fatalf("violations=%d, want 2", v)
	}
}

func TestSupervisorUnitDriftIsViolation(t *testing.T) {
	f := &fakeCollector{id: "f", samples: []*types.MetricSample{sample("x", "share")}}
	s, _ := newTestSupervisor(t, member("f", f))
	if out, _ := s.Collect(); len(out) != 1 {
		t.Fatalf("tick 1: %d samples, want 1", len(out))
	}
	f.set([]*types.MetricSample{sample("x", "bytes")}, nil)
	if out, _ := s.Collect(); len(out) != 0 {
		t.Fatalf("tick 2: drifted unit delivered")
	}
	if v := statusOf(t, s, "f").Violations; v != 1 {
		t.Fatalf("violations=%d, want 1", v)
	}
}

func TestSupervisorRangeDriftIsViolation(t *testing.T) {
	ranged := sample("x", "share")
	ranged.Range = &[2]float64{0, 1}
	f := &fakeCollector{id: "f", samples: []*types.MetricSample{ranged}}
	s, _ := newTestSupervisor(t, member("f", f))
	s.Collect()

	unranged := sample("x", "share")
	wider := sample("x", "share")
	wider.Range = &[2]float64{0, 100}
	f.set([]*types.MetricSample{unranged, wider}, nil)
	if out, _ := s.Collect(); len(out) != 0 {
		t.Fatalf("drifted range delivered %d samples", len(out))
	}
	if v := statusOf(t, s, "f").Violations; v != 2 {
		t.Fatalf("violations=%d, want 2", v)
	}
}

func TestSupervisorOwnershipIsStickyAcrossBackoff(t *testing.T) {
	a := &fakeCollector{id: "a", samples: []*types.MetricSample{sample("cpu_utilization", "share")}}
	scoped := sample("cpu_utilization", "share")
	scoped.Subject = "pod:x"
	b := &fakeCollector{id: "b", samples: []*types.MetricSample{sample("cpu_utilization", "share"), scoped}}
	s, _ := newTestSupervisor(t, member("a", a), member("b", b))

	out, _ := s.Collect()
	if len(out) != 2 || out[0].Subject != "" || out[1].Subject != "pod:x" {
		t.Fatalf("tick 1: want A's unscoped and B's scoped sample, got %d", len(out))
	}
	if d := statusOf(t, s, "b").OwnershipDrops; d != 1 {
		t.Fatalf("b ownership_drops=%d, want 1", d)
	}

	a.set(nil, errors.New("down"))
	out, _ = s.Collect()
	if len(out) != 1 || out[0].Subject != "pod:x" {
		t.Fatalf("tick 2 (A faulted): want only B's scoped sample, got %d", len(out))
	}
	// A is in backoff now and not even called; ownership must still hold.
	out, _ = s.Collect()
	if len(out) != 1 || out[0].Subject != "pod:x" {
		t.Fatalf("tick 3 (A in backoff): want only B's scoped sample, got %d", len(out))
	}
	if d := statusOf(t, s, "b").OwnershipDrops; d != 3 {
		t.Fatalf("b ownership_drops=%d, want 3", d)
	}
}

func TestSupervisorOwnershipDisplacedByEarlierMember(t *testing.T) {
	a := &fakeCollector{id: "a"}
	b := &fakeCollector{id: "b", samples: []*types.MetricSample{sample("t", "share")}}
	s, _ := newTestSupervisor(t, member("a", a), member("b", b))

	for i := 0; i < 2; i++ {
		if out, _ := s.Collect(); len(out) != 1 {
			t.Fatalf("tick %d: B alone emits t, want it kept", i+1)
		}
	}
	a.set([]*types.MetricSample{sample("t", "share")}, nil)
	out, _ := s.Collect()
	if len(out) != 1 {
		t.Fatalf("tick 3: %d samples, want 1", len(out))
	}
	if statusOf(t, s, "a").Samples != 1 || statusOf(t, s, "b").OwnershipDrops != 1 {
		t.Fatalf("tick 3: want A kept and B dropped")
	}
}

func TestSupervisorOutputInListOrder(t *testing.T) {
	a := &fakeCollector{id: "a", delay: 50 * time.Millisecond, samples: []*types.MetricSample{sample("p", "share")}}
	b := &fakeCollector{id: "b", samples: []*types.MetricSample{sample("q", "share")}}
	s, _ := newTestSupervisor(t, member("a", a), member("b", b))
	out, _ := s.Collect()
	if len(out) != 2 || out[0].MetricType != "p" || out[1].MetricType != "q" {
		t.Fatalf("output not in list order")
	}
}

func TestSupervisorSourceIDAndAvailable(t *testing.T) {
	a := &fakeCollector{id: "a", avail: []types.MetricType{"x", "y"}}
	b := &fakeCollector{id: "b", avail: []types.MetricType{"y", "z"}}
	s, _ := newTestSupervisor(t, member("a", a), member("b", b))
	if got := s.SourceID(); got != "supervisor:a,b" {
		t.Fatalf("SourceID = %q", got)
	}
	got := s.AvailableMetrics()
	want := []types.MetricType{"x", "y", "z"}
	if len(got) != len(want) {
		t.Fatalf("AvailableMetrics = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AvailableMetrics = %v, want %v", got, want)
		}
	}
}

func TestSupervisorStatusStartsAndReportsRestarts(t *testing.T) {
	r := &restartingFake{fakeCollector: fakeCollector{id: "r"}}
	r.restarts.Store(3)
	s, _ := newTestSupervisor(t, Member{Name: "r", Kind: "exec", C: r, Timeout: time.Second})
	st := statusOf(t, s, "r")
	if st.State != "starting" || st.Kind != "exec" || st.Restarts != 3 {
		t.Fatalf("status before first tick = %+v", st)
	}
}

func TestSupervisorCloseClosesClosers(t *testing.T) {
	r := &restartingFake{fakeCollector: fakeCollector{id: "r"}}
	plain := &fakeCollector{id: "p"}
	s, _ := newTestSupervisor(t, member("r", &r.fakeCollector), Member{Name: "c", Kind: "exec", C: r, Timeout: time.Second}, member("p", plain))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !r.closed.Load() {
		t.Fatal("closer member not closed")
	}
}

type restartingFake struct {
	fakeCollector
	restarts atomic.Int64
	closed   atomic.Bool
}

func (r *restartingFake) Restarts() int64 { return r.restarts.Load() }
func (r *restartingFake) Close() error    { r.closed.Store(true); return nil }

// An earlier member that is slow but on time can use up a later member's whole
// wait while the supervisor is still waiting on it. The later member answered in
// time; it must not be faulted because its result was collected late.
func TestSupervisorPromptMemberNotFaultedBehindSlowerOne(t *testing.T) {
	a := &fakeCollector{id: "a", delay: 150 * time.Millisecond, samples: []*types.MetricSample{sample("p", "share")}}
	b := &fakeCollector{id: "b", samples: []*types.MetricSample{sample("q", "share")}}
	ma, mb := member("a", a), member("b", b)
	ma.Timeout = 200 * time.Millisecond
	mb.Timeout = 50 * time.Millisecond
	s, _ := newTestSupervisor(t, ma, mb)
	for i := 0; i < 8; i++ {
		out, _ := s.Collect()
		if len(out) != 2 {
			t.Fatalf("tick %d: %d samples, want 2", i+1, len(out))
		}
		if st := statusOf(t, s, "b"); st.State != "ok" || st.LastError != "" {
			t.Fatalf("tick %d: b state=%q last_error=%q, want ok", i+1, st.State, st.LastError)
		}
	}
}

// droppingFake reports drops the way the exec and http adapters do.
type droppingFake struct {
	fakeCollector
	drops []contracts.SampleDrop
}

func (d *droppingFake) TakeDrops() []contracts.SampleDrop {
	out := d.drops
	d.drops = nil
	return out
}

// What an adapter dropped from its plugin's reply is a violation like any other:
// counted in status and logged once per kind and type.
func TestSupervisorCountsAdapterDropsAsViolations(t *testing.T) {
	d := &droppingFake{fakeCollector: fakeCollector{id: "d", samples: []*types.MetricSample{sample("ok", "share")}}}
	d.drops = []contracts.SampleDrop{{Kind: contracts.DropMalformed, MetricType: "x"}, {Kind: contracts.DropDuplicate, MetricType: "ok"}}
	sink := &logSink{}
	s := NewSupervisor(Env{NodeID: testNode, Logf: sink.logf}, []Member{{Name: "d", Kind: "exec", C: d, Timeout: time.Second}})
	out, _ := s.Collect()
	if len(out) != 1 {
		t.Fatalf("got %d samples, want the clean batch", len(out))
	}
	st := statusOf(t, s, "d")
	if st.Violations != 2 || st.Samples != 1 || st.State != "ok" {
		t.Fatalf("status = %+v, want 2 violations, 1 sample, ok", st)
	}
	if sink.count(`collector[d]: dropped "x": malformed sample`) != 1 || sink.count(`collector[d]: dropped "ok": duplicate sample`) != 1 {
		t.Fatalf("log = %q", sink.lines)
	}
}

// A violation is logged once per kind and metric type, not once per kind: the
// second type broken the same way is news an operator needs.
func TestSupervisorViolationLogKeyedByKindAndType(t *testing.T) {
	sink := &logSink{}
	f := &fakeCollector{id: "f", samples: []*types.MetricSample{sample("a", ""), sample("a", ""), sample("b", "")}}
	s := NewSupervisor(Env{NodeID: testNode, Logf: sink.logf}, []Member{member("f", f)})
	s.Collect()
	s.Collect()
	if n := sink.count(`collector[f]: dropped "a": missing unit`); n != 1 {
		t.Errorf("type a logged %d times, want 1", n)
	}
	if n := sink.count(`collector[f]: dropped "b": missing unit`); n != 1 {
		t.Errorf("type b logged %d times, want 1", n)
	}
	if v := statusOf(t, s, "f").Violations; v != 6 {
		t.Errorf("violations = %d, want 6", v)
	}
}

// A member built by hand without a timeout gets the one Build would give its entry,
// min(interval/2, 5s), not a fixed 5s.
func TestSupervisorHandBuiltMemberTimeoutFollowsInterval(t *testing.T) {
	s := NewSupervisor(Env{NodeID: testNode, Interval: 2 * time.Second}, []Member{
		{Name: "t", Kind: "type", C: &fakeCollector{id: "t"}},
		{Name: "x", Kind: "exec", C: &fakeCollector{id: "x"}},
	})
	if w := s.members[0].wait; w != time.Second {
		t.Errorf("type member waits %s, want 1s", w)
	}
	if w := s.members[1].wait; w != time.Second+outOfProcessGrace {
		t.Errorf("exec member waits %s, want 1s plus the grace", w)
	}
}

func manySamples(n int) []*types.MetricSample {
	out := make([]*types.MetricSample, n)
	for i := range out {
		out[i] = sample("cpu_utilization", "ratio")
	}
	return out
}

// TestSupervisorCapsSamplesPerTick: one producer cannot make a tick's admission
// unbounded; the excess is a counted "flood" violation.
func TestSupervisorCapsSamplesPerTick(t *testing.T) {
	f := &fakeCollector{id: "f", samples: manySamples(20000)}
	s, _ := newTestSupervisor(t, member("f", f))
	out, _ := s.Collect()
	if len(out) != 16384 {
		t.Fatalf("len(out) = %d; want 16384", len(out))
	}
	st := statusOf(t, s, "f")
	if st.Samples != 16384 || st.Violations != 3616 || st.ViolationKinds["flood"] != 3616 {
		t.Errorf("samples %d, violations %d, kinds %v; want 16384, 3616, flood 3616", st.Samples, st.Violations, st.ViolationKinds)
	}
}

// TestSupervisorCapIsInclusive: the 16384th sample passes; the 16385th is the flood.
func TestSupervisorCapIsInclusive(t *testing.T) {
	f := &fakeCollector{id: "f", samples: manySamples(16384)}
	s, _ := newTestSupervisor(t, member("f", f))
	out, _ := s.Collect()
	st := statusOf(t, s, "f")
	if len(out) != 16384 || st.Violations != 0 {
		t.Errorf("16384 samples: kept %d, violations %d; want 16384, 0", len(out), st.Violations)
	}
	if _, ok := st.ViolationKinds["flood"]; ok {
		t.Error("flood key present at exactly the cap")
	}

	f = &fakeCollector{id: "g", samples: manySamples(16385)}
	s, _ = newTestSupervisor(t, member("g", f))
	out, _ = s.Collect()
	st = statusOf(t, s, "g")
	if len(out) != 16384 || st.Violations != 1 {
		t.Errorf("16385 samples: kept %d, violations %d; want 16384, 1", len(out), st.Violations)
	}
}
