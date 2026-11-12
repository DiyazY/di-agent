package peers

import (
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
)

// ── Registry tests ───────────────────────────────────────────────────────────

func TestRegistry_AddIsIdempotentByURL(t *testing.T) {
	r := NewRegistry()
	first, err := r.Add("http://node_1:8080", "first")
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if first.Trust != 0.5 {
		t.Errorf("default trust: got %.3f, want 0.5", first.Trust)
	}
	// Bump trust so we can detect any silent reset on re-Add.
	if err := r.SetTrust(first.ID, 0.9); err != nil {
		t.Fatal(err)
	}
	second, err := r.Add("http://node_1:8080", "second")
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("Add must dedupe by URL: ids differ %s vs %s", first.ID, second.ID)
	}
	if second.Trust != 0.9 {
		t.Errorf("re-Add must not reset trust: got %.3f, want 0.9", second.Trust)
	}
	list, _ := r.List()
	if len(list) != 1 {
		t.Errorf("List after duplicate Add: got %d, want 1", len(list))
	}
}

func TestRegistry_AddEmptyURL(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Add("   ", "x"); !errors.Is(err, ErrEmptyURL) {
		t.Errorf("empty URL: got %v, want ErrEmptyURL", err)
	}
}

func TestRegistry_RemoveUnknownReturnsError(t *testing.T) {
	r := NewRegistry()
	if err := r.Remove("nope"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("Remove unknown: got %v, want ErrUnknownPeer", err)
	}
}

func TestRegistry_RemoveDeletesAndFreesURL(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "x")
	if err := r.Remove(d.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(d.ID)
	if got != nil {
		t.Error("Get after Remove: descriptor still present")
	}
	// URL must be reusable after removal.
	again, err := r.Add("http://node_1:8080", "y")
	if err != nil {
		t.Fatal(err)
	}
	if again.Note != "y" {
		t.Errorf("re-Add after Remove: note %q want %q", again.Note, "y")
	}
}

func TestRegistry_ListReturnsDefensiveCopies(t *testing.T) {
	r := NewRegistry()
	r.Add("http://node_1:8080", "a") //nolint:errcheck
	r.Add("http://node_2:8080", "b") //nolint:errcheck
	list, _ := r.List()
	if len(list) != 2 {
		t.Fatalf("List len: got %d, want 2", len(list))
	}
	// Mutate returned descriptor — registry must be unaffected.
	list[0].Trust = 0.001
	list[0].Note = "tampered"
	again, _ := r.List()
	if again[0].Trust == 0.001 {
		t.Error("registry leaks internal Descriptor — List did not defensively copy")
	}
	if again[0].Note == "tampered" {
		t.Error("registry leaks internal Descriptor — Note was mutated")
	}
}

func TestRegistry_UpdateTrustClampsAndIncrementsNObserved(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	// Push above 1.0 — must clamp.
	if err := r.UpdateTrust(d.ID, +5.0); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(d.ID)
	if got.Trust != 1.0 {
		t.Errorf("trust after +5: got %.3f, want 1.0 (clamped)", got.Trust)
	}
	if got.NObserved != 1 {
		t.Errorf("NObserved after one UpdateTrust: got %d, want 1", got.NObserved)
	}
	// Push below 0 — must clamp.
	if err := r.UpdateTrust(d.ID, -10.0); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Get(d.ID)
	if got.Trust != 0.0 {
		t.Errorf("trust after -10: got %.3f, want 0.0 (clamped)", got.Trust)
	}
	if got.NObserved != 2 {
		t.Errorf("NObserved after two UpdateTrust: got %d, want 2", got.NObserved)
	}
}

func TestRegistry_SetTrustOverride(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	if err := r.SetTrust(d.ID, 0.75); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(d.ID)
	if got.Trust != 0.75 {
		t.Errorf("trust after SetTrust(0.75): got %.3f", got.Trust)
	}
	// SetTrust above 1.0 clamps.
	if err := r.SetTrust(d.ID, 2.0); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Get(d.ID)
	if got.Trust != 1.0 {
		t.Errorf("trust after SetTrust(2.0): got %.3f (must clamp to 1.0)", got.Trust)
	}
}

func TestRegistry_MarkSeenSetsTimestamp(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	got, _ := r.Get(d.ID)
	if !got.LastSeen.IsZero() {
		t.Error("LastSeen should be zero at registration")
	}
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := r.MarkSeen(d.ID, when); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Get(d.ID)
	if !got.LastSeen.Equal(when) {
		t.Errorf("LastSeen: got %v, want %v", got.LastSeen, when)
	}
}

func TestRegistry_GetByURL(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	got, _ := r.GetByURL("http://node_1:8080")
	if got == nil || got.ID != d.ID {
		t.Errorf("GetByURL: got %+v, want id=%s", got, d.ID)
	}
	missing, _ := r.GetByURL("http://nope")
	if missing != nil {
		t.Errorf("GetByURL miss: got %+v, want nil", missing)
	}
}

func TestRegistry_IDStableAcrossRegistries(t *testing.T) {
	// Two independent registries that register the same URL must produce the
	// same ID — the property the daemon relies on across restarts.
	a := NewRegistry()
	b := NewRegistry()
	d1, _ := a.Add("http://node_1:8080", "")
	d2, _ := b.Add("http://node_1:8080", "")
	if d1.ID != d2.ID {
		t.Errorf("ID not stable across registries: %s vs %s", d1.ID, d2.ID)
	}
}

func TestRegistry_ConcurrentMutationsAreSafe(t *testing.T) {
	r := NewRegistry()
	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := "http://peer_" + itoa(i) + ":8080"
			d, _ := r.Add(url, "")
			_ = r.UpdateTrust(d.ID, 0.01)
			_ = r.MarkSeen(d.ID, time.Now())
		}(i)
	}
	wg.Wait()
	list, _ := r.List()
	if len(list) != N {
		t.Errorf("after concurrent Add: got %d, want %d", len(list), N)
	}
}

// ── Client tests (httptest fixtures) ─────────────────────────────────────────

func TestClient_CostSuccessDecodesActionCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/cost" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if q := r.URL.Query().Get("task"); q != "pod-scheduling" {
			t.Errorf("task=%q", q)
		}
		if q := r.URL.Query().Get("node"); q != "node_A" {
			t.Errorf("node=%q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ActionCost{
			CPUCost:         0.1,
			ResourceCost:    0.42,
			LatencyEstimate: 12.5,
			Confidence:      0.7,
			Rationale:       "peer rationale",
			GraphPathUsed:   []string{"SC→RC[P1](0.8)"},
		})
	}))
	defer srv.Close()

	c := NewClient(2 * time.Second)
	cost, err := c.Cost(context.Background(), srv.URL, "pod-scheduling", "node_A")
	if err != nil {
		t.Fatal(err)
	}
	if cost.ResourceCost != 0.42 {
		t.Errorf("ResourceCost: got %.3f, want 0.42", cost.ResourceCost)
	}
	if len(cost.GraphPathUsed) != 1 {
		t.Errorf("GraphPathUsed len: got %d, want 1", len(cost.GraphPathUsed))
	}
}

func TestClient_CostNon200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(2 * time.Second)
	if _, err := c.Cost(context.Background(), srv.URL, "x", "y"); err == nil {
		t.Error("expected error on 500; got nil")
	}
}

func TestClient_CostTransportErrorReturnsError(t *testing.T) {
	c := NewClient(200 * time.Millisecond)
	// Bind to a closed port — connect refused.
	if _, err := c.Cost(context.Background(), "http://127.0.0.1:1", "x", "y"); err == nil {
		t.Error("expected transport error on closed port; got nil")
	}
}

func TestClient_HealthRespectsStatus(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer okSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer badSrv.Close()

	c := NewClient(2 * time.Second)
	if !c.Health(context.Background(), okSrv.URL) {
		t.Error("Health on 200: got false, want true")
	}
	if c.Health(context.Background(), badSrv.URL) {
		t.Error("Health on 503: got true, want false")
	}
	if c.Health(context.Background(), "http://127.0.0.1:1") {
		t.Error("Health on closed port: got true, want false")
	}
}

// offerServer answers every POST /offers with status and body, and reports the
// offer it was sent.
func offerServer(t *testing.T, status int, body string, got *admission.Offer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/offers" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: got %q, want application/json", ct)
		}
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("decode offer: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestOffer_ReturnsThePeersHop(t *testing.T) {
	var got admission.Offer
	srv := offerServer(t, 200, `{"node":"A","url":"http://a","decision_id":"admit-1-1","verdict":"accept","reason":"fits"}`, &got)
	defer srv.Close()
	offer := admission.Offer{Name: "web", Demand: map[string]admission.Demand{"cpu": {Value: 0.5, Source: "declared"}}}
	hop, err := NewClient(2*time.Second).Offer(context.Background(), srv.URL+"/", offer)
	if err != nil {
		t.Fatal(err)
	}
	if hop.Node != "A" || hop.Verdict != admission.Accept || hop.DecisionID != "admit-1-1" || hop.Reason != "fits" {
		t.Errorf("hop %+v", hop)
	}
	if got.Name != "web" || got.Demand["cpu"].Value != 0.5 {
		t.Errorf("peer received %+v", got)
	}
}

func TestOffer_DeclineWithIDIsAnAnswer(t *testing.T) {
	srv := offerServer(t, 200, `{"node":"A","decision_id":"admit-1-2","verdict":"decline","reason":"relief active on cpu_contention_ratio"}`, nil)
	defer srv.Close()
	hop, err := NewClient(2*time.Second).Offer(context.Background(), srv.URL, admission.Offer{Name: "web"})
	if err != nil || hop.Verdict != admission.Decline {
		t.Errorf("hop %+v, err %v", hop, err)
	}
}

func TestOffer_Non2xxIsAnError(t *testing.T) {
	srv := offerServer(t, 500, `boom`, nil)
	defer srv.Close()
	if _, err := NewClient(2*time.Second).Offer(context.Background(), srv.URL, admission.Offer{Name: "web"}); err == nil {
		t.Error("500: got nil error")
	}
}

func TestOffer_TransportAndBodyErrors(t *testing.T) {
	c := NewClient(2 * time.Second)
	if _, err := c.Offer(context.Background(), "http://127.0.0.1:1", admission.Offer{Name: "web"}); err == nil {
		t.Error("closed port: got nil error")
	}
	srv := offerServer(t, 200, `not json`, nil)
	defer srv.Close()
	if _, err := c.Offer(context.Background(), srv.URL, admission.Offer{Name: "web"}); err == nil {
		t.Error("undecodable body: got nil error")
	}
}

// An answer that cannot be traced to a journalled decision is not an answer.
func TestOffer_AnswerWithoutDecisionIDIsAnError(t *testing.T) {
	for _, body := range []string{`{"verdict":"accept"}`, `{"verdict":"decline","reason":"x"}`} {
		srv := offerServer(t, 200, body, nil)
		_, err := NewClient(2*time.Second).Offer(context.Background(), srv.URL, admission.Offer{Name: "web"})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "no decision id") {
			t.Errorf("%s: err %v; want a no-decision-id error", body, err)
		}
	}
}

// A verdict that is not one of admission's is not an answer, even with an id.
func TestOffer_UnknownVerdictIsAnError(t *testing.T) {
	for _, body := range []string{`{}`, `{"decision_id":"admit-1-1","verdict":"maybe"}`, `{"decision_id":"admit-1-1","verdict":"ACCEPT"}`} {
		srv := offerServer(t, 200, body, nil)
		_, err := NewClient(2*time.Second).Offer(context.Background(), srv.URL, admission.Offer{Name: "web"})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "unknown verdict") {
			t.Errorf("%s: err %v; want an unknown-verdict error", body, err)
		}
	}
}

func TestOffer_AlreadyAskedNeedsNoID(t *testing.T) {
	srv := offerServer(t, 200, `{"node":"A","verdict":"already_asked","reason":"already in the chain"}`, nil)
	defer srv.Close()
	hop, err := NewClient(2*time.Second).Offer(context.Background(), srv.URL, admission.Offer{Name: "web"})
	if err != nil || hop.Verdict != admission.AlreadyAsked {
		t.Errorf("hop %+v, err %v", hop, err)
	}
}

func TestClient_CostContextCancellation(t *testing.T) {
	// Server blocks until context is done.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(0) // no client timeout — rely on ctx
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Cost(ctx, srv.URL, "x", "y"); err == nil {
		t.Error("expected error on context timeout; got nil")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// itoa is a tiny dependency-free int→string for test peer URLs.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ── Trust forces ─────────────────────────────────────────────────────────────

func trustNear(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestRegistry_PenaliseRecordsWhen(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := r.Penalise(d.ID, t0); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(d.ID)
	if !trustNear(got.Trust, 0.45) {
		t.Errorf("trust after Penalise: got %v, want 0.45", got.Trust)
	}
	if !got.LastPenalised.Equal(t0) {
		t.Errorf("LastPenalised: got %v, want %v", got.LastPenalised, t0)
	}
	if got.NObserved != 1 {
		t.Errorf("NObserved: got %d, want 1", got.NObserved)
	}
	if err := r.Penalise("nope", t0); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("Penalise unknown: got %v", err)
	}
}

func TestRegistry_RewardIsSymmetricAndCapped(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	_ = r.Penalise(d.ID, time.Now())
	if err := r.Reward(d.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(d.ID)
	if !trustNear(got.Trust, 0.50) || got.NObserved != 2 {
		t.Errorf("after reward: trust %v n %d, want 0.50 and 2", got.Trust, got.NObserved)
	}
	_ = r.SetTrust(d.ID, 0.98)
	_ = r.Reward(d.ID)
	got, _ = r.Get(d.ID)
	if got.Trust != 1.0 {
		t.Errorf("reward must cap at 1.0, got %v", got.Trust)
	}
	if err := r.Reward("nope"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("Reward unknown: got %v", err)
	}
}

func TestRegistry_ListCopiesLastPenalised(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = r.Penalise(d.ID, t0)
	list, _ := r.List()
	if len(list) != 1 || !list[0].LastPenalised.Equal(t0) {
		t.Errorf("List must carry LastPenalised: %+v", list)
	}
}

func TestRegistry_SetTrustClearsLastPenalised(t *testing.T) {
	r := NewRegistry()
	d, _ := r.Add("http://node_1:8080", "")
	_ = r.Penalise(d.ID, time.Now())
	_ = r.SetTrust(d.ID, 0.15)
	got, _ := r.Get(d.ID)
	if !got.LastPenalised.IsZero() {
		t.Errorf("an operator-set trust must clear LastPenalised, got %v", got.LastPenalised)
	}
}
