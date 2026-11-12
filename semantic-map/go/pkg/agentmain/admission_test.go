package agentmain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/admission"
	"github.com/DiyazY/di-agent/pkg/explain"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/profiles"
	"github.com/DiyazY/di-agent/pkg/relief"
	"github.com/DiyazY/di-agent/pkg/runtime"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

var admitT0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// The cpu.limit and memory.high levers' descriptors, at the spec's default levels.
var testDomains = []admission.Domain{
	{Action: spec.LeverCPULimit, Resource: "cpu", Relieves: "cpu_contention_ratio", Usage: "cpu_utilization",
		Capacity: "cpu_cores", Exit: 0.10, Enter: 0.25, Settle: 2 * time.Minute, Phase: "calm"},
	{Action: spec.LeverMemoryHigh, Resource: "memory", Relieves: "memory_contention_ratio", Usage: "memory_utilization",
		Capacity: "memory_total", Exit: 0.03, Enter: 0.08, Settle: 2 * time.Minute, Phase: "calm"},
}

// testAgent is one node: its own map, its own server, and nothing shared with
// the others but the URLs its peers func lists.
type testAgent struct {
	id    string
	a     *admitter
	j     *statemap.Journal
	srv   *httptest.Server
	peers []*peers.Descriptor
	// running is the workload listing, set before a request; listErr fails it.
	running map[string]string
	listErr error
}

// newAdmitNode is a 4-core, 8 GiB node at cpu usage 0.4 and cpu pressure p, with
// memory at 0.2 and no memory pressure. p = 0.02 accepts a small offer; p = 0.5 is
// past cpu.limit's enter level, so the gate declines.
func newAdmitNode(t *testing.T, id string, p float64) *testAgent {
	t.Helper()
	j := statemap.NewJournal(0)
	m := statemap.New(statemap.Config{AdmitUnknown: true, Alpha: 1}, j)
	m.SetClock(func() time.Time { return admitT0 })
	if err := m.RecordFacts("test", []statemap.Fact{statemap.NumberFact("cpu_cores", 4, "cores"),
		statemap.NumberFact("memory_total", 8<<30, "bytes")}); err != nil {
		t.Fatal(err)
	}
	unit := [2]float64{0, 1}
	for _, o := range []struct {
		id, subject string
		v           float64
	}{{"cpu_utilization", "", 0.4}, {"memory_utilization", "", 0.2}, {"memory_contention_ratio", "", 0},
		{"cpu_contention_ratio", "", p}, {"cpu_utilization@pod:a", "pod:a", 0.1}} {
		if err := m.Record(statemap.Observation{ID: o.id, Value: o.v, At: admitT0, Subject: o.subject, Range: &unit}); err != nil {
			t.Fatal(err)
		}
	}
	ta := &testAgent{id: id, j: j}
	ta.a = &admitter{
		m: m, self: admission.Self{NodeID: id, Arch: "amd64"},
		tracker: admission.NewTracker(admission.TrackerConfig{Map: m, Now: func() time.Time { return admitT0 },
			Shares: func(usage string) map[string]float64 { return relief.Shares(m, usage) },
			Acted:  func(string, time.Time) (float64, bool) { return 0, false }}),
		domains: func() []admission.Domain { return testDomains },
		workloads: func(context.Context) (map[string]string, error) {
			return ta.running, ta.listErr
		},
		peers:  func() ([]*peers.Descriptor, error) { return ta.peers, nil },
		client: peers.NewClient(5 * time.Second),
	}
	mux := http.NewServeMux()
	registerAdmissionRoutes(mux, ta.a)
	ta.srv = httptest.NewServer(mux)
	t.Cleanup(ta.srv.Close)
	return ta
}

func (ta *testAgent) knows(others ...*testAgent) {
	for _, o := range others {
		ta.peers = append(ta.peers, &peers.Descriptor{ID: strings.ToLower(o.id), URL: o.srv.URL})
	}
}

func (ta *testAgent) decisions() int { return len(ta.j.Decisions(0, "")) }

func smallOffer(name string) admission.Offer {
	return admission.Offer{Name: name, Demand: map[string]admission.Demand{
		"cpu":    {Value: 0.4, Source: "declared"},
		"memory": {Value: 64 << 20, Source: "declared"},
	}}
}

func postOffer(t *testing.T, url string, v any) *http.Response {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func submit(t *testing.T, ta *testAgent, o admission.Offer) submission {
	t.Helper()
	resp := postOffer(t, ta.srv.URL+"/tasks", o)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /tasks: %d", resp.StatusCode)
	}
	var s submission
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func verdicts(chain []admission.Hop) string {
	var v []string
	for _, h := range chain {
		v = append(v, h.Node+":"+h.Verdict)
	}
	return strings.Join(v, " ")
}

func TestSubmitForwardsUntilAccept(t *testing.T) {
	a, b, c := newAdmitNode(t, "A", 0.5), newAdmitNode(t, "B", 0.02), newAdmitNode(t, "C", 0.02)
	a.knows(b, c)
	s := submit(t, a, smallOffer("web"))
	if got := verdicts(s.Chain); got != "A:decline B:accept" {
		t.Fatalf("chain %s; want A:decline B:accept\n%+v", got, s.Chain)
	}
	if s.AcceptedBy == nil || s.AcceptedBy.Node != "B" || s.AcceptedBy.URL != b.srv.URL {
		t.Fatalf("accepted_by %+v; want B at %s", s.AcceptedBy, b.srv.URL)
	}
	if s.DecisionID == "" || s.DecisionID != s.Chain[1].DecisionID {
		t.Fatalf("decision id %q; want B's %q", s.DecisionID, s.Chain[1].DecisionID)
	}
	if s.Chain[0].URL != "" || s.Chain[1].URL != b.srv.URL {
		t.Fatalf("hop urls %q %q; want self empty, then B's", s.Chain[0].URL, s.Chain[1].URL)
	}
	// Each hop's decision resolves on the node that decided.
	if _, ok := a.j.Decision(s.Chain[0].DecisionID); !ok {
		t.Fatalf("A's decline %s is not in A's journal", s.Chain[0].DecisionID)
	}
	if _, ok := b.j.Decision(s.DecisionID); !ok {
		t.Fatalf("B's accept %s is not in B's journal", s.DecisionID)
	}
	if c.decisions() != 0 {
		t.Fatalf("C was asked: %d decisions", c.decisions())
	}
	if !b.a.tracker.Pending("web") || a.a.tracker.Pending("web") {
		t.Fatal("the accept should be pending on B only")
	}

	// GET /tasks lists the submission with its chain on A, and on B the accept it
	// made for a peer, pending before any tick has judged it.
	list := listTasks(t, a)
	if len(list.Submissions) != 1 || list.Submissions[0].DecisionID != s.DecisionID || len(list.Submissions[0].Chain) != 2 {
		t.Fatalf("GET /tasks %+v", list)
	}
	if list.Answered["decline"] != 1 || list.Answered["accept"] != 0 || len(list.Pending) != 0 {
		t.Fatalf("A answered %v pending %v; want one decline, nothing pending", list.Answered, list.Pending)
	}
	lb := listTasks(t, b)
	if len(lb.Submissions) != 0 || lb.Answered["accept"] != 1 {
		t.Fatalf("B submissions %d answered %v; want none and one accept", len(lb.Submissions), lb.Answered)
	}
	if len(lb.Pending) != 1 || lb.Pending[0].Name != "web" || lb.Pending[0].AcceptID != s.DecisionID ||
		lb.Pending[0].Arrived || !lb.Pending[0].AcceptedAt.Equal(admitT0) || len(lb.Outcomes) != 0 {
		t.Fatalf("B pending %+v outcomes %+v; want web awaiting arrival, no outcome", lb.Pending, lb.Outcomes)
	}
	if lb.Prediction.Decisions != 1 || lb.Prediction.BasisEvaluations != 2 ||
		lb.Prediction.ByBasis["usage"] != 2 || lb.Prediction.Accepted != 1 ||
		lb.Prediction.ChangedByPrediction != 0 {
		t.Fatalf("B prediction summary %+v; want one accepted decision on two usage bases", lb.Prediction)
	}
	if list.Prediction.Decisions != 0 {
		t.Fatalf("A prediction summary %+v; gate decline should not reach prediction", list.Prediction)
	}
}

type taskPrediction struct {
	Decisions           int                         `json:"decisions"`
	BasisEvaluations    int                         `json:"basis_evaluations"`
	ByBasis             map[string]int              `json:"by_basis"`
	Accepted            int                         `json:"accepted"`
	Declined            int                         `json:"declined"`
	ChangedByPrediction int                         `json:"changed_by_prediction"`
	OutcomeErrors       admission.PredictionSummary `json:"outcome_errors"`
}

type taskList struct {
	Submissions []submission              `json:"submissions"`
	Answered    map[string]int            `json:"answered"`
	Pending     []admission.PendingAccept `json:"pending"`
	Outcomes    []admission.Outcome       `json:"outcomes"`
	Prediction  taskPrediction            `json:"prediction"`
}

func listTasks(t *testing.T, ta *testAgent) taskList {
	t.Helper()
	resp, err := http.Get(ta.srv.URL + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var l taskList
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestAllDecline(t *testing.T) {
	a, b, c := newAdmitNode(t, "A", 0.5), newAdmitNode(t, "B", 0.5), newAdmitNode(t, "C", 0.5)
	a.knows(b, c)
	s := submit(t, a, smallOffer("web"))
	if s.AcceptedBy != nil || s.DecisionID != "" {
		t.Fatalf("accepted_by %+v %q; want none", s.AcceptedBy, s.DecisionID)
	}
	if got := verdicts(s.Chain); got != "A:decline B:decline C:decline" {
		t.Fatalf("chain %s", got)
	}
	for _, h := range s.Chain {
		if h.DecisionID == "" || h.Reason == "" {
			t.Fatalf("hop without decision or reason: %+v", h)
		}
	}
	for _, n := range []*testAgent{a, b, c} {
		tasks := listTasks(t, n)
		if got := tasks.Answered; len(got) != 1 || got["decline"] != 1 {
			t.Fatalf("%s answered %v; want one decline", n.id, got)
		}
		if tasks.Prediction.Decisions != 0 {
			t.Fatalf("%s prediction summary %+v; relief/headroom gate declines must not count as prediction", n.id, tasks.Prediction)
		}
	}
}

func TestPredictionDeclineIsCountedAsChangingTheVerdict(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	if err := a.a.m.DeclareRelationship(statemap.Relationship{
		From: "cpu_utilization@pod:a", To: "cpu_contention_ratio", Sign: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.a.m.RecordIntervention(statemap.InterventionOutcome{
		From: "cpu_utilization@pod:a", To: "cpu_contention_ratio", Agreed: true,
		HasEffect: true, Effect: 0.9, DecisionID: "verify-effect",
	}); err != nil {
		t.Fatal(err)
	}

	o := smallOffer("web")
	o.Demand["cpu"] = admission.Demand{Value: 1.8, Source: "declared"}
	s := submit(t, a, o)
	if s.Chain[0].Verdict != admission.Decline || !strings.Contains(s.Chain[0].Reason, "map basis") {
		t.Fatalf("hop %+v; want prediction-stage map decline", s.Chain[0])
	}
	prediction := listTasks(t, a).Prediction
	if prediction.Decisions != 1 || prediction.ByBasis["map"] != 1 ||
		prediction.ByBasis["usage"] != 1 || prediction.Declined != 1 ||
		prediction.ChangedByPrediction != 1 {
		t.Fatalf("prediction summary %+v; want one map-driven decline beyond the gates", prediction)
	}
}

func TestMissingPredictionInputsDoNotCountAsBasisOrChangedVerdict(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	a.a.domains = func() []admission.Domain {
		return []admission.Domain{{
			Resource: "cpu", Relieves: "cpu_contention_ratio", Usage: "cpu_utilization",
			Capacity: "missing_capacity", Exit: 0.10, Enter: 0.25, Phase: "calm",
		}}
	}

	s := submit(t, a, smallOffer("web"))
	if s.Chain[0].Verdict != admission.Decline {
		t.Fatalf("hop %+v; missing capacity should decline", s.Chain[0])
	}
	prediction := listTasks(t, a).Prediction
	if prediction.Decisions != 1 || prediction.BasisEvaluations != 0 ||
		len(prediction.ByBasis) != 0 || prediction.Declined != 1 ||
		prediction.ChangedByPrediction != 0 {
		t.Fatalf("prediction summary %+v; missing inputs are not a basis or a prediction-driven decline", prediction)
	}
}

func TestMemoryHeadroomDeclineIsNotCountedAsPrediction(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	unit := [2]float64{0, 1}
	if err := a.a.m.Record(statemap.Observation{
		ID: "memory_admission_headroom_ratio", Value: 0, At: admitT0, Range: &unit,
	}); err != nil {
		t.Fatal(err)
	}
	s := submit(t, a, smallOffer("web"))
	if s.Chain[0].Verdict != admission.Decline || !strings.Contains(s.Chain[0].Reason, "memory demand share") {
		t.Fatalf("hop %+v; want memory-headroom decline", s.Chain[0])
	}
	prediction := listTasks(t, a).Prediction
	if prediction.Decisions != 1 || prediction.Declined != 1 ||
		prediction.ChangedByPrediction != 0 || prediction.BasisEvaluations != 1 ||
		len(prediction.ByBasis) != 1 || prediction.ByBasis["usage"] != 1 {
		t.Fatalf("summary %+v; only CPU's usage basis should be counted", prediction)
	}
}

func TestPeerDown(t *testing.T) {
	a, b, c := newAdmitNode(t, "A", 0.5), newAdmitNode(t, "B", 0.02), newAdmitNode(t, "C", 0.02)
	a.knows(b, c)
	b.srv.Close()
	s := submit(t, a, smallOffer("web"))
	if len(s.Chain) != 3 || s.Chain[1].Verdict != admission.Unreachable || s.Chain[1].URL != b.srv.URL || s.Chain[1].Reason == "" {
		t.Fatalf("chain %+v; want B unreachable with the error", s.Chain)
	}
	if s.AcceptedBy == nil || s.AcceptedBy.Node != "C" {
		t.Fatalf("accepted_by %+v; want C", s.AcceptedBy)
	}
}

func TestChainLoop(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	o := smallOffer("web")
	o.Chain = []admission.Hop{{Node: "A", Verdict: admission.Decline, DecisionID: "admit-1-1"}}
	resp := postOffer(t, a.srv.URL+"/offers", o)
	var hop admission.Hop
	if err := json.NewDecoder(resp.Body).Decode(&hop); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || hop.Verdict != admission.AlreadyAsked || hop.Node != "A" || hop.DecisionID != "" {
		t.Fatalf("%d %+v; want already_asked from A, undecided", resp.StatusCode, hop)
	}
	if a.decisions() != 0 {
		t.Fatalf("%d decisions recorded; want none", a.decisions())
	}

	// By URL: a peer listed twice is asked once.
	s, b := newAdmitNode(t, "S", 0.5), newAdmitNode(t, "B", 0.5)
	s.peers = []*peers.Descriptor{{ID: "b1", URL: b.srv.URL}, {ID: "b2", URL: b.srv.URL}}
	sub := submit(t, s, smallOffer("web"))
	if got := verdicts(sub.Chain); got != "S:decline B:decline" {
		t.Fatalf("chain %s; want B once", got)
	}
}

func TestRegistryContainsSelf(t *testing.T) {
	a := newAdmitNode(t, "A", 0.5)
	a.knows(a)
	s := submit(t, a, smallOffer("web"))
	if got := verdicts(s.Chain); got != "A:decline A:already_asked" {
		t.Fatalf("chain %s; want A:decline A:already_asked", got)
	}
	if ans := listTasks(t, a).Answered; ans["decline"] != 1 || ans["already_asked"] != 1 {
		t.Fatalf("answered %v; want one decline and one already_asked", ans)
	}
	if a.decisions() != 2 { // the local decline and its gate
		t.Fatalf("%d decisions; want the local decline and its gate only", a.decisions())
	}
}

func TestMalformedOffer(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	bad := []admission.Offer{
		{Demand: map[string]admission.Demand{"cpu": {Value: 1}}},
		{Name: "web", Demand: map[string]admission.Demand{"cpu": {Value: -1}}},
		{Name: "web", Demand: map[string]admission.Demand{"gpu": {Value: 1}}},
	}
	for _, path := range []string{"/tasks", "/offers"} {
		for _, o := range bad {
			if resp := postOffer(t, a.srv.URL+path, o); resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("POST %s %+v: %d; want 400", path, o, resp.StatusCode)
			}
		}
		resp, err := http.Post(a.srv.URL+path, "application/json", strings.NewReader("{not json"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST %s garbage: %d; want 400", path, resp.StatusCode)
		}
	}
	if a.decisions() != 0 {
		t.Fatalf("%d decisions recorded; want none", a.decisions())
	}
}

func TestPeerAnswerWithoutID(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, admission.Hop{Node: "B", Verdict: admission.Accept})
	}))
	defer stub.Close()
	a := newAdmitNode(t, "A", 0.5)
	a.peers = []*peers.Descriptor{{ID: "b", URL: stub.URL}}
	s := submit(t, a, smallOffer("web"))
	if len(s.Chain) != 2 || s.Chain[1].Verdict != admission.Unreachable || s.AcceptedBy != nil {
		t.Fatalf("chain %+v, accepted_by %+v; want B unreachable and nobody accepting", s.Chain, s.AcceptedBy)
	}
}

func TestPeerOrderDeterministic(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	stub := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, name)
			mu.Unlock()
			writeJSON(w, admission.Hop{Node: name, DecisionID: "admit-1-1", Verdict: admission.Decline, Reason: "busy"})
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	x, y, z := stub("x"), stub("y"), stub("z")
	a := newAdmitNode(t, "A", 0.5)
	a.peers = []*peers.Descriptor{{ID: "3", URL: z.URL}, {ID: "1", URL: x.URL}, {ID: "2", URL: y.URL}}
	submit(t, a, smallOffer("web"))
	if got := strings.Join(asked, ","); got != "x,y,z" {
		t.Fatalf("asked %s; want x,y,z", got)
	}
}

// TestAlreadyRunningHere: a name already running on the node is declined with a
// recorded decision, before Decide, as is any offer when the listing fails.
func TestAlreadyRunningHere(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	a.running = map[string]string{"web": "pod:web"}
	s := submit(t, a, smallOffer("web"))
	h := s.Chain[0]
	if h.Verdict != admission.Decline || h.Reason != "already running here" || s.AcceptedBy != nil {
		t.Fatalf("hop %+v; want decline already running here", h)
	}
	if d, ok := a.j.Decision(h.DecisionID); !ok || d.Answer["reason"] != "already running here" {
		t.Fatalf("decline %s: recorded %v %+v", h.DecisionID, ok, d)
	}
	if a.a.tracker.Pending("web") {
		t.Fatal("a running name registered for an outcome")
	}

	b := newAdmitNode(t, "B", 0.02)
	b.listErr = errors.New("boom")
	resp := postOffer(t, b.srv.URL+"/offers", smallOffer("web"))
	var hop admission.Hop
	if err := json.NewDecoder(resp.Body).Decode(&hop); err != nil {
		t.Fatal(err)
	}
	if hop.Verdict != admission.Decline || hop.Reason != "workload list unavailable: boom" || hop.DecisionID == "" {
		t.Fatalf("hop %+v; want decline on the listing error", hop)
	}
}

func TestConcurrentSubmitSameName(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	var wg sync.WaitGroup
	got := make([]submission, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(smallOffer("web"))
			resp, err := http.Post(a.srv.URL+"/tasks", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if err := json.NewDecoder(resp.Body).Decode(&got[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	accepts, pending := 0, 0
	for _, s := range got {
		h := s.Chain[0]
		switch {
		case h.Verdict == admission.Accept:
			accepts++
		case h.Verdict == admission.Decline && h.Reason == "already pending":
			pending++
			// The decline is a decision of its own, never the accept it lost to.
			d, ok := a.j.Decision(h.DecisionID)
			if !ok || d.Answer["verdict"] != admission.Decline {
				t.Fatalf("decline %s: recorded %v %+v", h.DecisionID, ok, d)
			}
		}
	}
	if accepts != 1 || pending != 1 {
		t.Fatalf("chains %+v; want one accept and one already pending", got)
	}
	// A race-lost accept is answered, and counted, as a decline.
	if ans := listTasks(t, a).Answered; ans["accept"] != 1 || ans["decline"] != 1 {
		t.Fatalf("answered %v; want one accept and one decline", ans)
	}
}

func TestOffloadGone(t *testing.T) {
	sm, err := profiles.Build("edge-minimal", profiles.Config{StateMap: newTestState(t)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, sm, explain.NewDisabled())
	registerAdmissionRoutes(mux, newAdmitNode(t, "A", 0.02).a)
	req := httptest.NewRequest(http.MethodPost, "/offload", strings.NewReader(`{"workload":"web"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /offload: %d; want 404 or 405", rec.Code)
	}
}

func TestReliefDomains(t *testing.T) {
	if got := reliefDomains(nil)(); len(got) != 0 {
		t.Fatalf("relief off: %+v; want no domains", got)
	}
	_, loop, dr, acting := actingMuxWith(t, true)
	doms := reliefDomains([]*relief.Loop{loop})()
	d, l := loop.Descriptor(), loop.Lever()
	want := admission.Domain{Action: d.Action, Resource: d.Resource, Relieves: d.Relieves, Usage: d.Usage,
		Capacity: d.Capacity, Exit: l.Exit, Enter: l.Enter, Settle: l.Settle, Phase: string(loop.Status().Phase)}
	if len(doms) != 1 || doms[0] != want {
		t.Fatalf("domains %+v; want [%+v]", doms, want)
	}
	if want.Action != spec.LeverCPULimit || want.Relieves != "cpu_contention_ratio" || want.Phase != "calm" || want.Settle == 0 {
		t.Fatalf("unexpected cpu.limit domain %+v", want)
	}

	actedFn := reliefActed([]*relief.Loop{loop})
	acted := func(action string, since time.Time) bool { _, ok := actedFn(action, since); return ok }
	if acted(spec.LeverCPULimit, time.Time{}) {
		t.Fatal("acted before any action")
	}
	c := runtime.Workload{Subject: "pod:c", Name: "c", Resources: map[string]runtime.Resource{"cpu": {Limit: f64(0.5)}}}
	dr.Put(c)
	if _, err := acting.Apply(context.Background(), c, spec.LeverCPULimit, 0.25, time.Now().Add(time.Hour), "act-1"); err != nil {
		t.Fatal(err)
	}
	if err := loop.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	at := loop.Actions()[0].ActedAt
	if !acted(spec.LeverCPULimit, at.Add(-time.Second)) || acted(spec.LeverCPULimit, at) || acted(spec.LeverMemoryHigh, at.Add(-time.Second)) {
		t.Fatal("acted should be true only for cpu.limit, since before its action")
	}
	if peak, _ := actedFn(spec.LeverCPULimit, at.Add(-time.Second)); peak != loop.Actions()[0].ContentionAtAct {
		t.Fatalf("peak %v; want the action's contention at act %v", peak, loop.Actions()[0].ContentionAtAct)
	}
}

// TestStrictOffer: a misspelt field or trailing data is refused, not read as a
// zero demand that could be accepted.
func TestStrictOffer(t *testing.T) {
	a := newAdmitNode(t, "A", 0.02)
	for _, path := range []string{"/tasks", "/offers"} {
		for _, body := range []string{
			`{"name":"web","demand":{"cpu":{"vaule":2},"memory":{"value":1}}}`,
			`{"name":"web","demand":{"cpu":{"value":0.4},"memory":{"value":1}}} {}`,
		} {
			resp, err := http.Post(a.srv.URL+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("POST %s %s: %d; want 400", path, body, resp.StatusCode)
			}
		}
	}
	if a.decisions() != 0 {
		t.Fatalf("%d decisions recorded; want none", a.decisions())
	}
}

// TestPeerTimeoutStopsWalk: a peer that times out may have accepted, so the walk
// stops there rather than risk a second accept, and says the outcome is unknown.
func TestPeerTimeoutStopsWalk(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, admission.Hop{Node: "B", DecisionID: "admit-1-1", Verdict: admission.Accept})
	}))
	defer slow.Close()
	c := newAdmitNode(t, "C", 0.02)
	a := newAdmitNode(t, "A", 0.5)
	a.a.client = peers.NewClient(50 * time.Millisecond)
	a.peers = []*peers.Descriptor{{ID: "1", URL: slow.URL}, {ID: "2", URL: c.srv.URL}}
	s := submit(t, a, smallOffer("web"))
	if len(s.Chain) != 2 || s.Chain[1].URL != slow.URL || s.Chain[1].Verdict != admission.Unreachable ||
		!strings.HasPrefix(s.Chain[1].Reason, "outcome unknown") || s.AcceptedBy != nil {
		t.Fatalf("chain %+v accepted_by %+v; want the walk to stop at B, outcome unknown", s.Chain, s.AcceptedBy)
	}
	if c.decisions() != 0 {
		t.Fatalf("C was asked: %d decisions", c.decisions())
	}
}

// TestReliefCalmPinned: admission compares a domain's phase to "calm" without
// importing relief.
func TestReliefCalmPinned(t *testing.T) {
	if relief.Calm != "calm" {
		t.Fatalf("relief.Calm is %q; admission.Decide compares the phase to \"calm\"", relief.Calm)
	}
}
