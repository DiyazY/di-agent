package minimal_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/types"
)

type trustFixture struct {
	reg  *peers.Registry
	d    *peers.Descriptor
	fail *atomic.Bool
	hits *atomic.Int64
	now  *time.Time
	ask  func() error
}

func newTrustFixture(t *testing.T) *trustFixture {
	t.Helper()
	f := &trustFixture{fail: &atomic.Bool{}, hits: &atomic.Int64{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(peers.ActionCost{ResourceCost: 0.01})
	}))
	t.Cleanup(srv.Close)
	f.reg = peers.NewRegistry()
	f.d, _ = f.reg.Add(srv.URL, "")
	r := minimal.NewRuleEngineReasoner(mustSpec(), 0.5, f.reg, peers.NewClient(time.Second))
	// A loaded node, so a peer at cost 0.01 saves something and can be recommended.
	state := stateFor(t)
	spec := mustSpec()
	for _, rt := range spec.MetricRouting {
		if rt.ConstructID == spec.CostModel.ResourceConstruct {
			if err := state.Observe(rt.MetricType, 0.9, time.Now()); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	r.AttachState(state)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.now = &now
	r.SetClock(func() time.Time { return *f.now })
	f.ask = func() error {
		_, err := r.RecommendPeer(&types.OffloadContext{TaskType: "pod-scheduling", SourceNodeID: "node_self"})
		return err
	}
	return f
}

func (f *trustFixture) peer(t *testing.T) *peers.Descriptor {
	t.Helper()
	d, _ := f.reg.Get(f.d.ID)
	return d
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestAPenalisedPeerIsReprobedAfterTenMinutes(t *testing.T) {
	f := newTrustFixture(t)
	f.fail.Store(true)
	_ = f.ask()
	if f.hits.Load() != 1 {
		t.Fatalf("hits %d, want 1", f.hits.Load())
	}
	if d := f.peer(t); !near(d.Trust, 0.45) || !d.LastPenalised.Equal(*f.now) {
		t.Fatalf("after failure: %+v", d)
	}
	f.fail.Store(false)
	err := f.ask()
	if f.hits.Load() != 1 {
		t.Fatalf("a freshly penalised peer was queried: hits %d", f.hits.Load())
	}
	if !errors.Is(err, contracts.ErrInsufficientTrust) || !strings.Contains(err.Error(), "1 peers below trust floor") {
		t.Fatalf("err = %v", err)
	}
	*f.now = f.now.Add(10 * time.Minute)
	if err := f.ask(); err != nil {
		t.Fatalf("a re-probed peer back at the floor was not recommended: %v", err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("hits %d, want 2 after ten minutes", f.hits.Load())
	}
	if d := f.peer(t); !near(d.Trust, 0.50) || !d.LastSeen.Equal(*f.now) {
		t.Fatalf("after reprobe: %+v", d)
	}
	_ = f.ask()
	if f.hits.Load() != 3 {
		t.Fatalf("hits %d, want 3 (eligible by trust)", f.hits.Load())
	}
}

func TestAPeerThatFailsAgainWaitsAgain(t *testing.T) {
	f := newTrustFixture(t)
	f.fail.Store(true)
	_ = f.ask()
	*f.now = f.now.Add(10 * time.Minute)
	_ = f.ask()
	if f.hits.Load() != 2 {
		t.Fatalf("hits %d, want 2", f.hits.Load())
	}
	if d := f.peer(t); !near(d.Trust, 0.40) || !d.LastPenalised.Equal(*f.now) {
		t.Fatalf("after second failure: %+v", d)
	}
	*f.now = f.now.Add(5 * time.Minute)
	_ = f.ask()
	if f.hits.Load() != 2 {
		t.Fatalf("hits %d, want still 2", f.hits.Load())
	}
}

func TestAnOperatorSetFloorIsNotReprobed(t *testing.T) {
	f := newTrustFixture(t)
	_ = f.reg.SetTrust(f.d.ID, 0.15)
	*f.now = f.now.Add(time.Hour)
	err := f.ask()
	if f.hits.Load() != 0 {
		t.Fatalf("hits %d, want 0", f.hits.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "1 peers below trust floor") {
		t.Fatalf("err = %v", err)
	}
}

func TestARewardStopsAtOne(t *testing.T) {
	f := newTrustFixture(t)
	_ = f.reg.SetTrust(f.d.ID, 0.98)
	_ = f.ask()
	if d := f.peer(t); d.Trust != 1.0 {
		t.Fatalf("trust %v, want 1.0", d.Trust)
	}
}

func TestAnOperatorFloorOverAnAgentPenaltyIsNotReprobed(t *testing.T) {
	f := newTrustFixture(t)
	f.fail.Store(true)
	_ = f.ask()
	_ = f.reg.SetTrust(f.d.ID, 0.15)
	f.fail.Store(false)
	*f.now = f.now.Add(time.Hour)
	err := f.ask()
	if f.hits.Load() != 1 {
		t.Fatalf("hits %d, want 1", f.hits.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "1 peers below trust floor") {
		t.Fatalf("err = %v", err)
	}
}

func TestAReprobedPeerStillBelowTheFloorIsNotRecommended(t *testing.T) {
	f := newTrustFixture(t)
	f.fail.Store(true)
	for i := 0; i < 4; i++ { // 0.50 → 0.30, one penalty per retry wait
		_ = f.ask()
		*f.now = f.now.Add(10 * time.Minute)
	}
	if d := f.peer(t); !near(d.Trust, 0.30) {
		t.Fatalf("precondition: trust %v, want 0.30", d.Trust)
	}
	f.fail.Store(false)
	err := f.ask()
	if f.hits.Load() != 5 {
		t.Fatalf("hits %d, want 5: the stamp expired, so it is re-probed", f.hits.Load())
	}
	if d := f.peer(t); !near(d.Trust, 0.35) {
		t.Errorf("trust %v after a successful re-probe, want 0.35", d.Trust)
	}
	if !errors.Is(err, contracts.ErrInsufficientTrust) || !strings.Contains(err.Error(), "1 peers below trust floor") {
		t.Fatalf("a peer at 0.35 under a 0.5 floor was recommended or misreported: err = %v", err)
	}
}

// TestARestoredPenalisedPeerIsReprobed: a peer the agent penalised under the
// floor keeps its penalty time across a restart, so the restarted agent
// re-probes it after the retry window instead of reading it as operator-set.
func TestARestoredPenalisedPeerIsReprobed(t *testing.T) {
	a := newTrustFixture(t)
	a.fail.Store(true)
	_ = a.ask()
	saved := a.reg.Trusts()[a.d.ID]

	b := newTrustFixture(t) // the restarted agent, its peer at another test URL
	b.reg.RestoreTrust(map[string]peers.TrustState{b.d.ID: saved})
	if d := b.peer(t); !near(d.Trust, 0.45) || !d.LastPenalised.Equal(*a.now) {
		t.Fatalf("restored %+v; want trust 0.45 penalised at %v", d, *a.now)
	}
	_ = b.ask()
	if b.hits.Load() != 0 {
		t.Fatalf("hits %d inside the retry window, want 0", b.hits.Load())
	}
	*b.now = b.now.Add(10 * time.Minute)
	if err := b.ask(); err != nil || b.hits.Load() != 1 {
		t.Fatalf("hits %d err %v; want the restored peer re-probed after ten minutes", b.hits.Load(), err)
	}
}
