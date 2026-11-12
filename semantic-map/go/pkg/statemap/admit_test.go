package statemap

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// admitFixture: a 4-core, 8 GiB node at 40% CPU and 50% memory, with neither kind
// of contention, and the default relief levers' enter levels.
func admitFixture(t *testing.T) (*Map, *clock) {
	t.Helper()
	m, c := newTestMap(t, Config{AdmitUnknown: true, StaleAfter: time.Minute})
	if err := m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores"), NumberFact("memory_total", 8<<30, "bytes")}); err != nil {
		t.Fatal(err)
	}
	for id, v := range map[string]float64{
		"cpu_utilization": 0.4, "memory_utilization": 0.5,
		"cpu_contention_ratio": 0.05, "memory_contention_ratio": 0.01,
	} {
		if err := m.Record(Observation{ID: id, Value: v, At: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	return m, c
}

func admitReq(cpu, mem float64) AdmitRequest {
	return AdmitRequest{CPUCores: cpu, MemoryBytes: mem, CPUEnter: 0.25, MemoryEnter: 0.08}
}

func nearPtr(got *float64, want float64) bool { return got != nil && near(*got, want) }

// recordPod records pod:w's shares now; a nil value leaves that share unrecorded.
func recordPod(t *testing.T, m *Map, at time.Time, cpu, mem *float64) {
	t.Helper()
	unit := [2]float64{0, 1}
	for id, v := range map[string]*float64{"cpu_utilization@pod:w": cpu, "memory_utilization@pod:w": mem} {
		if v == nil {
			continue
		}
		if err := m.Record(Observation{ID: id, Value: *v, At: at, Subject: "pod:w", Range: &unit}); err != nil {
			t.Fatal(err)
		}
	}
}

// ageOut moves the clock past StaleAfter, re-records the node-level properties not in
// skip, and sweeps: what was not re-recorded goes stale.
func ageOut(t *testing.T, m *Map, c *clock, skip ...string) {
	t.Helper()
	c.advance(5 * time.Minute)
	for _, id := range []string{"cpu_utilization", "memory_utilization", "cpu_contention_ratio", "memory_contention_ratio"} {
		if slices.Contains(skip, id) {
			continue
		}
		p, _ := m.Property(id)
		if err := m.Record(Observation{ID: id, Value: p.Value, At: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	m.Sweep()
}

func ptr(v float64) *float64 { return &v }

func hasCaveat(cs []string, sub string) bool {
	for _, c := range cs {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func TestAdmitAcceptsWithHeadroom(t *testing.T) {
	m, _ := admitFixture(t)
	res := m.Admit(admitReq(1, 1<<30))
	if res.Err != "" || !res.Accepted {
		t.Fatalf("%+v; want accepted", res)
	}
	// 0.4 + 1/4 = 0.65 ; 0.5 + 1/8 = 0.625
	if !nearPtr(res.CPUAfter, 0.65) || !nearPtr(res.MemoryAfter, 0.625) || res.Basis != BasisDeclared {
		t.Errorf("%+v; want cpu_after 0.65, memory_after 0.625, declared", res)
	}
	if !hasCaveat(res.Caveats, "declared by the requester") {
		t.Errorf("caveats %v; a declared demand must say it was not observed", res.Caveats)
	}
	if !strings.HasPrefix(res.DecisionID, "adm-") {
		t.Errorf("decision id %q; want adm-<revision>-<digest>", res.DecisionID)
	}
}

func TestAdmitDeclinesOnCPUHeadroom(t *testing.T) {
	m, _ := admitFixture(t)
	res := m.Admit(admitReq(2.5, 0)) // 0.4 + 0.625 > 0.9
	if res.Accepted || !strings.Contains(res.Reason, "cpu") || res.CPUAfter == nil || *res.CPUAfter <= 0.9 {
		t.Errorf("%+v; want declined on cpu with cpu_after past 0.9", res)
	}
}

func TestAdmitDeclinesOnMemoryHeadroom(t *testing.T) {
	m, _ := admitFixture(t)
	res := m.Admit(admitReq(0, 4<<30)) // 0.5 + 0.5 > 0.9
	if res.Accepted || !strings.Contains(res.Reason, "memory") || res.MemoryAfter == nil || *res.MemoryAfter <= 0.9 {
		t.Errorf("%+v; want declined on memory with memory_after past 0.9", res)
	}
}

func TestAdmitDeclinesWhenContended(t *testing.T) {
	m, c := admitFixture(t)
	for range 5 {
		c.advance(time.Second)
		if err := m.Record(Observation{ID: "cpu_contention_ratio", Value: 0.9, At: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	if p, _ := m.Property("cpu_contention_ratio"); p.Value < 0.25 {
		t.Fatalf("fixture: contention %.3f did not reach enter", p.Value)
	}
	res := m.Admit(admitReq(0.1, 0))
	if res.Accepted || !strings.Contains(res.Reason, "contended") {
		t.Errorf("%+v; want declined as contended", res)
	}
}

func TestAdmitSkipsContentionWithoutALever(t *testing.T) {
	m, c := admitFixture(t)
	_ = m.Record(Observation{ID: "cpu_contention_ratio", Value: 0.9, At: c.now()})
	req := admitReq(0.1, 0)
	req.CPUEnter = 0
	res := m.Admit(req)
	if !res.Accepted || !hasCaveat(res.Caveats, "no relief lever") {
		t.Errorf("%+v; want accepted with the missing lever caveated", res)
	}
}

func TestAdmitMeasuresAStaleSubject(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.3), ptr(0.1))
	// The node's own readings keep arriving; the workload's have stopped.
	ageOut(t, m, c)
	if p, _ := m.Property("cpu_utilization@pod:w"); p.Status != Stale {
		t.Fatalf("fixture: pod:w is %s, want stale", p.Status)
	}
	req := admitReq(0.1, 0) // declared below what was measured: the measured shares are priced
	req.Workload = "pod:w"
	res := m.Admit(req)
	if !res.Accepted || res.Basis != BasisMeasured || !nearPtr(res.CPUAfter, 0.7) || !nearPtr(res.MemoryAfter, 0.6) {
		t.Fatalf("%+v; want accepted on the measured 0.3/0.1", res)
	}
	if hasCaveat(res.Caveats, "exceeded the measured") {
		t.Errorf("caveats %v; the declared demand did not exceed the measured one", res.Caveats)
	}
	if !hasCaveat(res.Caveats, "measured when it last ran here, 5m0s ago") {
		t.Errorf("caveats %v; want the age of the measurement", res.Caveats)
	}
	d, _ := m.Journal().Decision(res.DecisionID)
	read := map[string]bool{}
	for _, p := range d.PropertiesRead {
		read[p.ID] = true
	}
	if !read["cpu_utilization@pod:w"] || !read["memory_utilization@pod:w"] {
		t.Errorf("properties read %v; want the workload's shares the demand came from", d.PropertiesRead)
	}
}

// TestAdmitAnActiveSubjectIsAlreadyRunning: a pod still active here is already in the
// node's shares, so there is nothing to admit; it is declined, with no basis, since
// no demand was priced.
func TestAdmitAnActiveSubjectIsAlreadyRunning(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.3), ptr(0.1))
	res := m.Admit(AdmitRequest{Workload: "pod:w", CPUEnter: 0.25, MemoryEnter: 0.08})
	if res.Accepted || res.Reason != "pod:w is already running here (cpu_utilization@pod:w is active); nothing to admit" ||
		res.Basis != "" || res.CPUAfter != nil || res.MemoryAfter != nil {
		t.Errorf("%+v; want declined as already running, with no basis and no projection", res)
	}
	// A declared demand does not change that.
	req := admitReq(3, 4<<30)
	req.Workload = "pod:w"
	if res := m.Admit(req); res.Accepted || !strings.Contains(res.Reason, "already running") || res.Basis != "" {
		t.Errorf("%+v; want already running whatever was declared", res)
	}
}

// TestAdmitAStaleMeasuredPodCanDeclineOnHeadroom: the measured demand is priced like
// any other, and can be too much.
func TestAdmitAStaleMeasuredPodCanDeclineOnHeadroom(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.6), ptr(0.1))
	ageOut(t, m, c)
	res := m.Admit(AdmitRequest{Workload: "pod:w", CPUEnter: 0.25, MemoryEnter: 0.08})
	if res.Accepted || res.Basis != BasisMeasured || res.CPUAfter == nil || *res.CPUAfter <= 0.9 {
		t.Errorf("%+v; want declined on the measured 0.6 of CPU", res)
	}
}

// TestAdmitPricesTheLargerOfMeasuredAndDeclared: a requester declaring more than was
// measured may know the workload has grown; the larger is priced, and both recorded.
func TestAdmitPricesTheLargerOfMeasuredAndDeclared(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.1), ptr(0.1))
	ageOut(t, m, c)
	req := admitReq(1, 0) // 1/4 = 0.25 of CPU > the measured 0.1; memory 0 < 0.1
	req.Workload = "pod:w"
	res := m.Admit(req)
	if !res.Accepted || res.Basis != BasisMeasured || !nearPtr(res.CPUAfter, 0.65) || !nearPtr(res.MemoryAfter, 0.6) {
		t.Fatalf("%+v; want cpu priced on the declared 0.25, memory on the measured 0.1", res)
	}
	if !hasCaveat(res.Caveats, "declared cpu demand 0.2500 exceeded the measured 0.1000") ||
		hasCaveat(res.Caveats, "declared memory demand") {
		t.Errorf("caveats %v; want only the cpu override caveated", res.Caveats)
	}
	d, _ := m.Journal().Decision(res.DecisionID)
	if _, ok := d.Inputs["demand"]; !ok {
		t.Errorf("inputs %v; want both demands recorded", d.Inputs)
	}
}

// TestAdmitAPartlyMeasuredPod: the cgroup collector reports a pod's memory from its
// first tick and its CPU only from its second, so the map can hold one share alone.
func TestAdmitAPartlyMeasuredPod(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), nil, ptr(0.1))
	req := admitReq(3, 0)
	req.Workload = "pod:w"
	// Active, one share: already in the node's totals, nothing to admit.
	if res := m.Admit(req); res.Accepted || !strings.Contains(res.Reason, "already running") {
		t.Fatalf("%+v; want already running on the active memory share alone", res)
	}
	// Stale, one share: priced on the declared demand, naming the missing share.
	ageOut(t, m, c)
	req = admitReq(1, 0)
	req.Workload = "pod:w"
	res := m.Admit(req)
	if !res.Accepted || res.Basis != BasisDeclared || !nearPtr(res.CPUAfter, 0.65) {
		t.Fatalf("%+v; want accepted on the declared demand", res)
	}
	if !hasCaveat(res.Caveats, "holds memory_utilization@pod:w but not cpu_utilization@pod:w") {
		t.Errorf("caveats %v; want the missing share named", res.Caveats)
	}
	// Nothing declared: the reason says what the map has, not that it has nothing.
	res = m.Admit(AdmitRequest{Workload: "pod:w", CPUEnter: 0.25, MemoryEnter: 0.08})
	if res.Accepted || !strings.Contains(res.Reason, "nothing to price") || strings.Contains(res.Reason, "has not seen") ||
		!strings.Contains(res.Reason, "not cpu_utilization@pod:w") {
		t.Errorf("%+v; want nothing to price, naming the missing share", res)
	}
}

// TestAdmitAGroupIsPricedOnItsDeclaredDemand: every node has group:system, so its
// shares here are this node's own, not the requester's workload.
func TestAdmitAGroupIsPricedOnItsDeclaredDemand(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.3), ptr(0.1))
	if err := m.DeclareGroup("group:system", []string{"pod:w"}, nil); err != nil {
		t.Fatal(err)
	}
	req := admitReq(1, 0)
	req.Workload = "group:system"
	res := m.Admit(req)
	if !res.Accepted || strings.Contains(res.Reason, "already running") || res.Basis != BasisDeclared || !nearPtr(res.CPUAfter, 0.65) {
		t.Fatalf("%+v; want priced on the declared demand", res)
	}
	if !hasCaveat(res.Caveats, "group:system is not a pod") {
		t.Errorf("caveats %v; want why the group was not measured", res.Caveats)
	}
	res = m.Admit(AdmitRequest{Workload: "group:system", CPUEnter: 0.25, MemoryEnter: 0.08})
	if res.Accepted || !strings.Contains(res.Reason, "nothing to price") || !strings.Contains(res.Reason, "not a pod") {
		t.Errorf("%+v; want nothing to price for a group with no declared demand", res)
	}
}

// TestAdmitAlreadyRunningIsAnsweredBeforeContention: a workload already here has
// nothing to admit, and that is the answer, not the node's contention.
func TestAdmitAlreadyRunningIsAnsweredBeforeContention(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.3), ptr(0.1))
	for range 5 {
		c.advance(time.Second)
		_ = m.Record(Observation{ID: "cpu_contention_ratio", Value: 0.9, At: c.now()})
	}
	if p, _ := m.Property("cpu_contention_ratio"); p.Value < 0.25 {
		t.Fatalf("fixture: contention %.3f did not reach enter", p.Value)
	}
	res := m.Admit(AdmitRequest{Workload: "pod:w", CPUEnter: 0.25, MemoryEnter: 0.08})
	if res.Accepted || !strings.Contains(res.Reason, "already running") || strings.Contains(res.Reason, "contended") {
		t.Errorf("%+v; want declined as already running, not as contended", res)
	}
}

func TestAdmitDeclinesOnWhatTheMapLacks(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	res := m.Admit(admitReq(1, 0))
	if res.Accepted || !strings.Contains(res.Reason, "cpu_cores") || res.DecisionID == "" {
		t.Errorf("no facts: %+v; want a recorded decline naming cpu_cores", res)
	}
	_ = m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores"), NumberFact("memory_total", 8<<30, "bytes")})
	_ = m.Record(Observation{ID: "cpu_utilization", Value: 0.1, At: c.now()})
	res = m.Admit(admitReq(1, 0))
	if res.Accepted || !strings.Contains(res.Reason, "memory_utilization") {
		t.Errorf("no node memory reading: %+v; want a decline naming memory_utilization", res)
	}
	_ = m.Record(Observation{ID: "memory_utilization", Value: 0.1, At: c.now()})
	res = m.Admit(admitReq(0, 0))
	if res.Accepted || !strings.Contains(res.Reason, "nothing to price") {
		t.Errorf("no demand: %+v; want nothing to price", res)
	}
}

func TestAdmitIsReproducibleFromTheJournal(t *testing.T) {
	m, _ := admitFixture(t)
	res := m.Admit(admitReq(1, 1<<30))
	d, ok := m.Journal().Decision(res.DecisionID)
	if !ok {
		t.Fatalf("decision %s not in the journal", res.DecisionID)
	}
	read := map[string]float64{}
	for _, p := range d.PropertiesRead {
		read[p.ID] = p.Value
	}
	if read["cpu_utilization"] != 0.4 || read["memory_utilization"] != 0.5 {
		t.Errorf("properties read %v; want the node shares the projection used", read)
	}
	facts := map[string]bool{}
	for _, f := range d.FactsRead {
		facts[f.Key] = true
	}
	if !facts["cpu_cores"] || !facts["memory_total"] {
		t.Errorf("facts read %v; want cpu_cores and memory_total", d.FactsRead)
	}
	if d.Answer["accepted"] != true || d.Answer["basis"] != string(BasisDeclared) || !near(d.Answer["cpu_after"].(float64), 0.65) {
		t.Errorf("answer %v; want the result recorded", d.Answer)
	}
}

func TestAdmitRefusesAReusedID(t *testing.T) {
	m, _ := admitFixture(t)
	req := admitReq(1, 0)
	req.ID = "mine"
	if res := m.Admit(req); res.Err != "" {
		t.Fatal(res.Err)
	}
	if res := m.Admit(req); !strings.Contains(res.Err, "already used") {
		t.Errorf("%+v; want the reused id refused", res)
	}
	// Two different questions at one revision get two ids.
	a, b := m.Admit(admitReq(1, 0)), m.Admit(admitReq(2, 0))
	if a.DecisionID == b.DecisionID {
		t.Errorf("two requests share decision id %s", a.DecisionID)
	}
}

func TestAdmitRefusesABadDemand(t *testing.T) {
	m, _ := admitFixture(t)
	before := len(m.Journal().Decisions(0, ""))
	for _, req := range []AdmitRequest{admitReq(-1, 0), admitReq(math.NaN(), 0), admitReq(0, math.Inf(1))} {
		if res := m.Admit(req); res.Err == "" || res.DecisionID != "" {
			t.Errorf("%+v; want the demand refused before a decision", res)
		}
	}
	if after := len(m.Journal().Decisions(0, "")); after != before {
		t.Errorf("journal holds %d decisions, was %d; a refused request must record nothing", after, before)
	}
}

// TestAdmitAReusedExplicitIDRecordsNothing: an explicit id already used is refused,
// and the refusal does not touch the journal.
func TestAdmitAReusedExplicitIDRecordsNothing(t *testing.T) {
	m, _ := admitFixture(t)
	req := admitReq(1, 0)
	req.ID = "est-1-gate"
	if res := m.Admit(req); res.Err != "" || res.DecisionID != "est-1-gate" {
		t.Fatalf("%+v; want the explicit id kept", res)
	}
	before := len(m.Journal().Decisions(0, ""))
	if res := m.Admit(req); !strings.Contains(res.Err, "already used") {
		t.Errorf("%+v; want the reused id refused", res)
	}
	if after := len(m.Journal().Decisions(0, "")); after != before {
		t.Errorf("journal holds %d decisions, was %d", after, before)
	}
}

// TestAdmitIDsAreUnambiguousAndNeverOverwrite: two requests whose fields would join
// to the same text get different digests, and an identical retry gets a different
// id, so both records stay.
func TestAdmitIDsAreUnambiguousAndNeverOverwrite(t *testing.T) {
	m, _ := admitFixture(t)
	a := m.Admit(AdmitRequest{From: "n|pod:a", Workload: "b", CPUCores: 1, CPUEnter: 0.25, MemoryEnter: 0.08})
	b := m.Admit(AdmitRequest{From: "n", Workload: "pod:a|b", CPUCores: 1, CPUEnter: 0.25, MemoryEnter: 0.08})
	digest := func(id string) string { return id[strings.LastIndex(id, "-")+1:] }
	if digest(a.DecisionID) == digest(b.DecisionID) {
		t.Errorf("ids %s and %s; want two different digests", a.DecisionID, b.DecisionID)
	}
	first, retry := m.Admit(admitReq(1, 0)), m.Admit(admitReq(1, 0))
	if first.Revision != retry.Revision || retry.DecisionID == first.DecisionID ||
		digest(retry.DecisionID) != digest(first.DecisionID) {
		t.Fatalf("ids %s and %s at revisions %d/%d; want two ids over one digest", first.DecisionID, retry.DecisionID, first.Revision, retry.Revision)
	}
	for _, id := range []string{first.DecisionID, retry.DecisionID} {
		if _, ok := m.Journal().Decision(id); !ok {
			t.Errorf("decision %s was overwritten", id)
		}
	}
}

// TestAdmitSaysWhichDemandWasNotDeclared: a declared demand of one resource only is
// priced as zero of the other, and the decision says so.
func TestAdmitSaysWhichDemandWasNotDeclared(t *testing.T) {
	m, _ := admitFixture(t)
	if res := m.Admit(admitReq(1, 0)); !hasCaveat(res.Caveats, "no memory demand was declared") || hasCaveat(res.Caveats, "no cpu demand") {
		t.Errorf("caveats %v; want memory named", res.Caveats)
	}
	if res := m.Admit(admitReq(0, 1<<30)); !hasCaveat(res.Caveats, "no cpu demand was declared") {
		t.Errorf("caveats %v; want cpu named", res.Caveats)
	}
}

// TestAdmitDeclinesOnARetiredNodeShare: a retired reading is a reading of the past.
func TestAdmitDeclinesOnARetiredNodeShare(t *testing.T) {
	m, _ := admitFixture(t)
	if err := m.RetireProperty("memory_utilization", "test", "test"); err != nil {
		t.Fatal(err)
	}
	if res := m.Admit(admitReq(1, 0)); res.Accepted || res.Reason != "the map's reading of node memory_utilization is retired" {
		t.Errorf("%+v; want declined naming the retired share", res)
	}
}

// TestAdmitARetiredPodShareFallsBackToDeclared: a pod share retired is not a
// measurement, so the declared demand is priced, and the caveat says why.
func TestAdmitARetiredPodShareFallsBackToDeclared(t *testing.T) {
	m, c := admitFixture(t)
	recordPod(t, m, c.now(), ptr(0.3), ptr(0.1))
	ageOut(t, m, c)
	if err := m.RetireProperty("cpu_utilization@pod:w", "test", "test"); err != nil {
		t.Fatal(err)
	}
	req := admitReq(1, 0)
	req.Workload = "pod:w"
	res := m.Admit(req)
	if !res.Accepted || res.Basis != BasisDeclared || !hasCaveat(res.Caveats, "cpu_utilization@pod:w is retired") {
		t.Errorf("%+v; want priced on the declared demand, naming the retired share", res)
	}
	// The retired share's last value was not what was priced; no caveat may say it was.
	if hasCaveat(res.Caveats, "last value was used") || !hasCaveat(res.Caveats, "cpu_utilization@pod:w is retired (test); its last value was not used") {
		t.Errorf("caveats %q; want the retired share's value said to be unused", res.Caveats)
	}
}

func TestAdmitDeclinesWhenMemoryContended(t *testing.T) {
	m, c := admitFixture(t)
	for range 5 {
		c.advance(time.Second)
		if err := m.Record(Observation{ID: "memory_contention_ratio", Value: 0.9, At: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	res := m.Admit(admitReq(0.1, 0))
	if res.Accepted || !strings.Contains(res.Reason, "contended") || !strings.Contains(res.Reason, "memory_contention_ratio") {
		t.Errorf("%+v; want declined as memory contended", res)
	}
	if res.CPUAfter != nil {
		t.Errorf("cpu_after %v; a contention decline makes no projection", *res.CPUAfter)
	}
}

// TestAdmitBoundaries: headroom is "at or below", contention "at or above".
func TestAdmitBoundaries(t *testing.T) {
	m, _ := admitFixture(t)
	// Exact, not near: the fixture's readings were each recorded once, so the map holds
	// 0.4 and 0.5 as given; 2/4 is 0.5 and 3.2·2^30/2^33 is 0.4 in float64, and
	// 0.4 + 0.5 == 0.9 == 1 - admitHeadroom there. So these land on the limit itself,
	// which "at or below" must accept.
	if res := m.Admit(admitReq(2, 0)); !res.Accepted || !nearPtr(res.CPUAfter, 0.9) { // 0.4 + 2/4 = 0.9
		t.Errorf("%+v; want exactly 0.90 accepted", res)
	}
	if res := m.Admit(admitReq(2.001, 0)); res.Accepted {
		t.Errorf("%+v; want just above 0.90 declined", res)
	}
	if res := m.Admit(admitReq(0, 3.2*(1<<30))); !res.Accepted || !nearPtr(res.MemoryAfter, 0.9) { // 0.5 + 3.2/8
		t.Errorf("%+v; want memory at exactly 0.90 accepted", res)
	}

	for _, id := range []string{"cpu_contention_ratio", "memory_contention_ratio"} {
		m, c := newTestMap(t, Config{AdmitUnknown: true})
		_ = m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores"), NumberFact("memory_total", 8<<30, "bytes")})
		req := admitReq(0.1, 0)
		enter := map[string]float64{"cpu_contention_ratio": req.CPUEnter, "memory_contention_ratio": req.MemoryEnter}[id]
		for k, v := range map[string]float64{"cpu_utilization": 0.4, "memory_utilization": 0.5, id: enter} {
			if err := m.Record(Observation{ID: k, Value: v, At: c.now()}); err != nil {
				t.Fatal(err)
			}
		}
		if p, _ := m.Property(id); p.Value != enter {
			t.Fatalf("fixture: %s %.4f, want exactly %.4f", id, p.Value, enter)
		}
		if res := m.Admit(req); res.Accepted || !strings.Contains(res.Reason, id) {
			t.Errorf("%s exactly at enter: %+v; want declined", id, res)
		}
	}
}

func TestAdmitDeclinesOnAMissingOrNonPositiveFact(t *testing.T) {
	for name, facts := range map[string][]Fact{
		"memory_total missing": {NumberFact("cpu_cores", 4, "cores")},
		"cpu_cores zero":       {NumberFact("cpu_cores", 0, "cores"), NumberFact("memory_total", 8<<30, "bytes")},
		"cpu_cores negative":   {NumberFact("cpu_cores", -2, "cores"), NumberFact("memory_total", 8<<30, "bytes")},
	} {
		m, c := newTestMap(t, Config{AdmitUnknown: true})
		if err := m.RecordFacts("test", facts); err != nil {
			t.Fatal(err)
		}
		for id, v := range map[string]float64{"cpu_utilization": 0.4, "memory_utilization": 0.5} {
			_ = m.Record(Observation{ID: id, Value: v, At: c.now()})
		}
		want := "cpu_cores"
		if name == "memory_total missing" {
			want = "memory_total"
		}
		if res := m.Admit(admitReq(1, 1<<30)); res.Accepted || !strings.Contains(res.Reason, "no fact "+want) {
			t.Errorf("%s: %+v; want declined naming %s", name, res, want)
		}
	}
}

// TestAdmitDeclinesOnASeededNodeShare: a property declared by the specification and
// never observed holds an assumption, not a reading of the node.
func TestAdmitDeclinesOnASeededNodeShare(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores"), NumberFact("memory_total", 8<<30, "bytes")})
	if err := m.DeclareProperty(Property{ID: "cpu_utilization", Kind: Observed, Unit: "fraction", Range: [2]float64{0, 1}}); err != nil {
		t.Fatal(err)
	}
	_ = m.Record(Observation{ID: "memory_utilization", Value: 0.5, At: c.now()})
	if p, _ := m.Property("cpu_utilization"); p.NObservations != 0 {
		t.Fatalf("fixture: cpu_utilization has %d observations", p.NObservations)
	}
	if res := m.Admit(admitReq(1, 0)); res.Accepted || !strings.Contains(res.Reason, "no reading of node cpu_utilization") {
		t.Errorf("%+v; want declined on the unobserved cpu_utilization", res)
	}
}

func TestAdmitDeclinesOnAStaleNodeShare(t *testing.T) {
	for _, id := range []string{"cpu_utilization", "memory_utilization"} {
		m, c := admitFixture(t)
		ageOut(t, m, c, id)
		if p, _ := m.Property(id); p.Status != Stale {
			t.Fatalf("fixture: %s is %s, want stale", id, p.Status)
		}
		res := m.Admit(admitReq(0.1, 0))
		if res.Accepted || !strings.Contains(res.Reason, id) || !strings.Contains(res.Reason, "stale") {
			t.Errorf("stale %s: %+v; want declined naming it", id, res)
		}
	}
}

// TestAdmitAQuietContentionReading: a node contention reading that is stale or
// retired declines only while a pod still reports what it is the worst of. With none,
// the node has been emptied (the collector stops the CPU reading when no pod is
// promised), which is the best target, not a contended one.
func TestAdmitAQuietContentionReading(t *testing.T) {
	reporter := map[string]string{
		"cpu_contention_ratio":    "cpu_contention_ratio@pod:r",
		"memory_contention_ratio": "memory_pressure_ratio@pod:r",
	}
	for _, id := range []string{"cpu_contention_ratio", "memory_contention_ratio"} {
		for _, status := range []Status{Stale, Retired} {
			for _, reported := range []bool{false, true} {
				m, c := admitFixture(t)
				ageOut(t, m, c, id)
				if status == Retired {
					if err := m.RetireProperty(id, "test", "test"); err != nil {
						t.Fatal(err)
					}
				}
				if reported {
					unit := [2]float64{0, 1}
					if err := m.Record(Observation{ID: reporter[id], Value: 0.01, At: c.now(), Subject: "pod:r", Range: &unit}); err != nil {
						t.Fatal(err)
					}
				}
				if p, _ := m.Property(id); p.Status != status {
					t.Fatalf("fixture: %s is %s, want %s", id, p.Status, status)
				}
				res := m.Admit(admitReq(0.1, 0))
				name := fmt.Sprintf("%s %s reported=%v", status, id, reported)
				if reported {
					if res.Accepted || !strings.Contains(res.Reason, id) || !strings.Contains(res.Reason, string(status)) ||
						!strings.Contains(res.Reason, reporter[id]) {
						t.Errorf("%s: %+v; want declined naming the reading, its status and the reporter", name, res)
					}
					continue
				}
				if !res.Accepted || !hasCaveat(res.Caveats, id+" is "+string(status)+" and no workload reports it, so the node is taken as not contended") {
					t.Errorf("%s: %+v; want accepted with the quiet reading caveated", name, res)
				}
				if hasCaveat(res.Caveats, "last value was used") {
					t.Errorf("%s: caveats %q; the retired reading's value was not used", name, res.Caveats)
				}
			}
		}
	}
}

// TestAdmitSaysAContentionReadingIsMissing: no reading at all is a check not made,
// and the decision says so in its own words.
func TestAdmitSaysAContentionReadingIsMissing(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true})
	_ = m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores"), NumberFact("memory_total", 8<<30, "bytes")})
	_ = m.Record(Observation{ID: "cpu_utilization", Value: 0.4, At: c.now()})
	_ = m.Record(Observation{ID: "memory_utilization", Value: 0.5, At: c.now()})
	res := m.Admit(admitReq(1, 0))
	if !res.Accepted || !hasCaveat(res.Caveats, "cpu_contention_ratio has no reading, so cpu contention was not checked") ||
		!hasCaveat(res.Caveats, "memory_contention_ratio has no reading, so memory contention was not checked") {
		t.Errorf("%+v; want both missing readings caveated", res)
	}
}

// TestAdmitADeclineIsReproducibleFromTheJournal: a decline is recorded with its reason
// and the thresholds it was judged by.
func TestAdmitADeclineIsReproducibleFromTheJournal(t *testing.T) {
	m, _ := admitFixture(t)
	res := m.Admit(admitReq(2.5, 0))
	d, ok := m.Journal().Decision(res.DecisionID)
	if res.Accepted || !ok {
		t.Fatalf("%+v; want a journalled decline", res)
	}
	if d.Answer["accepted"] != false || d.Answer["reason"] != res.Reason || d.Answer["limit"] != 0.9 ||
		d.Answer["cpu_enter"] != 0.25 || d.Answer["memory_enter"] != 0.08 || !near(d.Answer["cpu_after"].(float64), 1.025) {
		t.Errorf("answer %v; want the decline, its reason and thresholds recorded", d.Answer)
	}
	if !strings.Contains(d.Rationale, res.Reason) {
		t.Errorf("rationale %q; want the reason in it", d.Rationale)
	}
}

func TestAdmitAnUnseenPodIsPricedOnItsDeclaredDemand(t *testing.T) {
	m, _ := admitFixture(t)
	req := admitReq(1, 0)
	req.Workload = "pod:new"
	res := m.Admit(req)
	if !res.Accepted || res.Basis != BasisDeclared || !hasCaveat(res.Caveats, "has not seen pod:new here") {
		t.Errorf("%+v; want priced on the declared demand, saying the pod is unseen", res)
	}
}

// TestAdmitIDsAreUniqueUnderConcurrentRequests: neither Decide nor Commit advances
// the revision and requests are served in parallel, so identical requests at one
// revision race for one name. Each must get its own id and its own record, and of
// concurrent requests under one explicit id exactly one may have it.
func TestAdmitIDsAreUniqueUnderConcurrentRequests(t *testing.T) {
	m, _ := admitFixture(t)
	const n = 16
	before := len(m.Journal().Decisions(0, ""))
	results := make([]AdmitResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = m.Admit(admitReq(1, 0)) }()
	}
	wg.Wait()
	ids := map[string]bool{}
	for _, r := range results {
		if r.Err != "" || ids[r.DecisionID] {
			t.Fatalf("%+v; want a fresh id per request (have %v)", r, ids)
		}
		ids[r.DecisionID] = true
		d, ok := m.Journal().Decision(r.DecisionID)
		if !ok || d.ID != r.DecisionID || d.Revision != r.Revision || d.Answer["accepted"] != r.Accepted {
			t.Errorf("decision %s: record %+v does not match what the caller got", r.DecisionID, d)
		}
	}
	if after := len(m.Journal().Decisions(0, "")); after != before+n {
		t.Errorf("journal holds %d decisions, was %d; want %d new records", after, before, n)
	}

	req := admitReq(1, 0)
	req.ID = "shared"
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = m.Admit(req) }()
	}
	wg.Wait()
	won := 0
	for _, r := range results {
		switch {
		case r.Err == "":
			won++
		case !strings.Contains(r.Err, "already used") || r.DecisionID != "":
			t.Errorf("%+v; want the id refused before a decision", r)
		}
	}
	if won != 1 {
		t.Errorf("%d requests got the explicit id; want exactly one", won)
	}
}

func TestAdmitDeclinesNamingAnUnobservableReading(t *testing.T) {
	m, c := newTestMap(t, Config{AdmitUnknown: true, StaleAfter: time.Minute})
	if err := m.RecordFacts("test", []Fact{NumberFact("cpu_cores", 4, "cores"), NumberFact("memory_total", 8<<30, "bytes")}); err != nil {
		t.Fatal(err)
	}
	for id, v := range map[string]float64{
		"cpu_utilization": 0.4, "memory_utilization": 0.5,
		"cpu_contention_ratio": 0.05, "memory_contention_ratio": 0.01,
	} {
		if err := m.Record(Observation{ID: id, Value: v, At: c.now(), Source: "cgroup"}); err != nil {
			t.Fatal(err)
		}
	}
	m.SetSourceDown("cgroup", true)
	m.Sweep()
	res := m.Admit(admitReq(1, 1<<30))
	if res.Accepted || !strings.Contains(res.Reason, "unobservable") {
		t.Errorf("%+v; want declined naming an unobservable reading", res)
	}
}
