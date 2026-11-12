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
	r.AttachState(stateFor(t))
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
	_ = f.ask()
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
