package explain_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/explain"
)

func TestSessionStore_CreateAndGet(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	s := store.Create()
	if s.ID == "" {
		t.Fatal("session ID must not be empty")
	}
	got, err := store.Get(s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("got ID %q; want %q", got.ID, s.ID)
	}
}

func TestSessionStore_UnknownIDReturnsErrNotFound(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	if _, err := store.Get("nonsense"); err != explain.ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound; got %v", err)
	}
}

func TestSessionStore_AppendTurnTrimsBuffer(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{MaxMessagesPerSes: 3})
	s := store.Create()
	for i := 0; i < 5; i++ {
		if err := store.AppendTurn(s.ID, "q", "turn-"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 3 {
		t.Errorf("expected 3 turns (buffer trimmed to MaxMessagesPerSes); got %d", len(got.Turns))
	}
	// The three surviving turns must be the most recent three.
	if got.Turns[0].Answer != "turn-2" || got.Turns[2].Answer != "turn-4" {
		t.Errorf("expected surviving turns to be turn-2..turn-4; got %+v", got.Turns)
	}
}

// Get must hand back a copy. Mutating it, or holding it across a concurrent
// AppendTurn, must not touch stored state — this is the contract that makes
// the unlocked read in explain() safe.
func TestSessionStore_GetReturnsDefensiveCopy(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	s := store.Create()
	if err := store.AppendTurn(s.ID, "q1", "a1"); err != nil {
		t.Fatal(err)
	}

	first, err := store.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the copy.
	first.Turns[0].Answer = "TAMPERED"
	first.Turns = append(first.Turns, explain.SessionTurn{Question: "ghost", Answer: "ghost"})

	second, err := store.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Turns) != 1 {
		t.Errorf("appending to a returned copy must not grow stored state; got %d turns", len(second.Turns))
	}
	if second.Turns[0].Answer != "a1" {
		t.Errorf("mutating a returned copy must not alter stored state; got %q", second.Turns[0].Answer)
	}
}

// The store is reachable from an HTTP handler, so every exported method must
// be safe under concurrent use. Run with -race.
func TestSessionStore_ConcurrentAccessIsRaceFree(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{MaxMessagesPerSes: 8})
	s := store.Create()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch n % 4 {
				case 0:
					_ = store.AppendTurn(s.ID, "q", "a")
				case 1:
					// The pattern explain() uses: Get, then iterate the turns
					// outside the lock while other goroutines append.
					if got, err := store.Get(s.ID); err == nil {
						for _, turn := range got.Turns {
							_ = turn.Answer
						}
					}
				case 2:
					args := map[string]any{"k": n}
					_ = store.CacheTool(s.ID, "get_peers", args, []byte(`[]`), "d")
					store.CachedTool(s.ID, "get_peers", args)
				case 3:
					store.InvalidateOnMutation(s.ID, time.Now())
					_ = store.Len()
				}
			}
		}(i)
	}
	wg.Wait()
}

// Idle sessions must be reclaimed, not merely ignored until LRU pressure.
func TestSessionStore_SweepsIdleSessionsOnCreate(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{
		MaxSessions:    50, // well above what we create, so LRU never fires
		SessionIdleTTL: 20 * time.Millisecond,
	})
	for i := 0; i < 5; i++ {
		store.Create()
	}
	if store.Len() != 5 {
		t.Fatalf("expected 5 live sessions; got %d", store.Len())
	}
	time.Sleep(40 * time.Millisecond)

	// Creating one more should sweep the five now-expired entries.
	store.Create()
	if got := store.Len(); got != 1 {
		t.Errorf("expected the 5 idle sessions to be swept, leaving 1; got %d", got)
	}
}

func TestSessionStore_ToolCacheHitAndTTL(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{
		ToolCacheTTL: 50 * time.Millisecond,
	})
	s := store.Create()
	args := map[string]any{"node_id": "master", "task_type": "pod-scheduling"}
	payload := json.RawMessage(`{"ResourceCost":0.035}`)
	if err := store.CacheTool(s.ID, "get_cost", args, payload, "cost node=master"); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.CachedTool(s.ID, "get_cost", args); !ok || string(got.Payload) != string(payload) {
		t.Errorf("expected cache hit; got ok=%v result=%+v", ok, got)
	}
	// TTL expiration.
	time.Sleep(80 * time.Millisecond)
	if _, ok := store.CachedTool(s.ID, "get_cost", args); ok {
		t.Errorf("expected cache miss after TTL; still hit")
	}
}

func TestSessionStore_InvalidateOnMutation(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{})
	s := store.Create()
	args := map[string]any{"proposition_id": "P1"}
	if err := store.CacheTool(s.ID, "get_edges", args, json.RawMessage(`[]`), "edges empty"); err != nil {
		t.Fatal(err)
	}
	// Watermark advances → cache must clear.
	store.InvalidateOnMutation(s.ID, time.Now().Add(1*time.Second))
	if _, ok := store.CachedTool(s.ID, "get_edges", args); ok {
		t.Errorf("expected cache flushed by mutation watermark; still hit")
	}
}

func TestSessionStore_LRUEvictionOnFull(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{MaxSessions: 2})
	a := store.Create()
	b := store.Create()
	c := store.Create() // evicts a (LRU)
	if _, err := store.Get(a.ID); err != explain.ErrSessionNotFound {
		t.Errorf("expected session %s to be evicted; err=%v", a.ID, err)
	}
	if _, err := store.Get(b.ID); err != nil {
		t.Errorf("session B should survive; err=%v", err)
	}
	if _, err := store.Get(c.ID); err != nil {
		t.Errorf("session C should exist; err=%v", err)
	}
}

func TestSessionStore_TouchOnGetKeepsSessionAlive(t *testing.T) {
	store := explain.NewSessionStore(explain.SessionConfig{MaxSessions: 2})
	a := store.Create()
	_ = store.Create()
	// Touch A so B becomes LRU.
	if _, err := store.Get(a.ID); err != nil {
		t.Fatal(err)
	}
	c := store.Create() // should evict B, not A
	if _, err := store.Get(a.ID); err != nil {
		t.Errorf("expected A to survive (touched most recently); err=%v", err)
	}
	if _, err := store.Get(c.ID); err != nil {
		t.Errorf("expected C to exist; err=%v", err)
	}
}
