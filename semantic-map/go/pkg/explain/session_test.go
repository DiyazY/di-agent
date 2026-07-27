package explain_test

import (
	"encoding/json"
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
		body := []byte(`{"answer":"turn-` + string(rune('0'+i)) + `"}`)
		if err := store.AppendTurn(s.ID, "q", json.RawMessage(body)); err != nil {
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
	if !containsAnswer(got.Turns[0].Response, "turn-2") ||
		!containsAnswer(got.Turns[2].Response, "turn-4") {
		t.Errorf("expected surviving turns to be turn-2..turn-4; got %+v", got.Turns)
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

// containsAnswer checks whether a raw JSON message has the given answer
// string. Small helper used by TestSessionStore_AppendTurnTrimsBuffer.
func containsAnswer(raw json.RawMessage, want string) bool {
	var wrap struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return false
	}
	return wrap.Answer == want
}
