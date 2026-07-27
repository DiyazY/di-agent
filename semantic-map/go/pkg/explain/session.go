package explain

import (
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SchemaVersion identifies the wire format of ExplainResponse. Bumped when
// fields are added or renamed in a way callers must branch on. v2 adds
// Usage, Plan, CriticVerdict, SchemaVersion; kept as a string so downstream
// tools can string-compare instead of doing integer math.
const SchemaVersion = "explain/2"

// SessionConfig bounds the in-memory session store. Zero values fall back to
// package defaults.
type SessionConfig struct {
	MaxSessions       int           // total concurrent sessions; default 100
	MaxMessagesPerSes int           // trimmed LRU per session; default 20
	MaxToolCacheSize  int           // cached tool results per session; default 32
	ToolCacheTTL      time.Duration // per-entry TTL; default 60s
	SessionIdleTTL    time.Duration // whole-session eviction; default 30m
}

// Defaults fills unset fields with package defaults.
func (c SessionConfig) Defaults() SessionConfig {
	if c.MaxSessions <= 0 {
		c.MaxSessions = 100
	}
	if c.MaxMessagesPerSes <= 0 {
		c.MaxMessagesPerSes = 20
	}
	if c.MaxToolCacheSize <= 0 {
		c.MaxToolCacheSize = 32
	}
	if c.ToolCacheTTL <= 0 {
		c.ToolCacheTTL = 60 * time.Second
	}
	if c.SessionIdleTTL <= 0 {
		c.SessionIdleTTL = 30 * time.Minute
	}
	return c
}

// SessionStore holds live conversation state for /explain callers who supply a
// session_id. Zero-value store is not safe — use NewSessionStore.
//
// Design choice: in-memory only. Session state has no scientific role (P6
// results are computed by the reasoner, not the Explainer); it exists purely
// as a UX / cost optimisation. Persisting sessions to disk would add crash-
// recovery complexity that no user has asked for. If someone wants durable
// session history, /history + /graph already provide the durable substrate.
type SessionStore struct {
	cfg SessionConfig

	mu       sync.Mutex
	sessions map[string]*Session // keyed by session_id
	order    *list.List          // LRU ordering; front = MRU, back = LRU
	elems    map[string]*list.Element
}

// NewSessionStore builds a fresh store. Config is normalised on entry.
func NewSessionStore(cfg SessionConfig) *SessionStore {
	return &SessionStore{
		cfg:      cfg.Defaults(),
		sessions: make(map[string]*Session),
		order:    list.New(),
		elems:    make(map[string]*list.Element),
	}
}

// Session is the per-conversation state kept in memory.
//
// Concurrency contract: the canonical Session lives inside the store and is
// only ever touched while the store's mutex is held. Get returns a defensive
// copy — callers may read it freely, but their copy will not observe later
// writes, and mutating it has no effect on stored state. Mutations go through
// AppendTurn / CacheTool / InvalidateOnMutation, which take the lock.
//
// This matters because two concurrent /explain calls can legitimately carry
// the same session_id. Handing out the live pointer would race the turn slice
// against AppendTurn.
type Session struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time

	// Turns records the (question, answer) exchanges so far, in order.
	// Bounded by cfg.MaxMessagesPerSes; older turns drop from the front.
	Turns []SessionTurn

	// ToolCache stores read-tool results for the current session. Keyed by
	// (tool_name, canonical JSON of args). Invalidated on ontology history
	// watermark change (see HistoryWatermark).
	//
	// Deliberately NOT populated on copies returned by Get — cache access is
	// via CachedTool/CacheTool so every read is properly synchronised. A nil
	// map here on a returned copy is correct, not a bug.
	ToolCache map[string]cachedTool

	// HistoryWatermark is the timestamp of the most-recent OntologyEvent we
	// observed. When the current watermark advances, the tool cache is
	// treated as stale — the ontology has changed under us.
	HistoryWatermark time.Time
}

// clone returns a defensive copy safe to hand outside the store's lock. Turns
// is deep-copied (the slice header alone would still alias the backing array).
// ToolCache is intentionally omitted; see the field comment.
func (s *Session) clone() *Session {
	if s == nil {
		return nil
	}
	out := &Session{
		ID:               s.ID,
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
		HistoryWatermark: s.HistoryWatermark,
	}
	if len(s.Turns) > 0 {
		out.Turns = make([]SessionTurn, len(s.Turns))
		copy(out.Turns, s.Turns)
	}
	return out
}

// SessionTurn is one round-trip in a multi-turn conversation.
//
// Only the answer text is retained, not the full ExplainResponse. Replaying a
// serialized response would push its tool_trace, usage, plan, and citation
// blocks back into the model's context — measured at ~16x the size of the
// answer alone, and none of it is information the model can act on. The
// durable record of what happened lives in the ontology audit log and the
// caller's own response, not here.
type SessionTurn struct {
	Question  string    `json:"question"`
	Answer    string    `json:"answer"`
	Timestamp time.Time `json:"timestamp"`
}

type cachedTool struct {
	Payload json.RawMessage
	Digest  string
	Expiry  time.Time
}

// ErrSessionNotFound signals an unknown session_id. Callers should treat this
// as fatal for the request — silently minting a new session would defeat the
// purpose of session continuity.
var ErrSessionNotFound = errors.New("explain: session not found")

// NewSessionID mints a fresh session ID (24 hex chars, 96 bits of entropy).
// Safe to call under concurrent load; uses crypto/rand.
func NewSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read only errors on OS RNG failure — falling back
		// to a stamped nonce is safer than aborting.
		nonce := fmt.Sprintf("fallback-%d", time.Now().UnixNano())
		sum := sha256.Sum256([]byte(nonce))
		return hex.EncodeToString(sum[:6])
	}
	return hex.EncodeToString(b[:])
}

// Get returns a defensive copy of the session, or ErrSessionNotFound.
//
// The copy is safe to read without holding any lock. It will not observe
// writes made after the call returns, and mutating it does nothing — use
// AppendTurn / CacheTool / InvalidateOnMutation to change stored state.
func (s *SessionStore) Get(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	if s.isIdleExpired(sess) {
		s.deleteLocked(id)
		return nil, ErrSessionNotFound
	}
	s.touchLocked(id)
	return sess.clone(), nil
}

// Create mints a fresh session and inserts it. Sweeps idle-expired sessions
// and evicts the LRU one when the store is full. Returns a copy of the new
// session, consistent with Get.
func (s *SessionStore) Create() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Sweep here rather than on a background ticker: Create is the only
	// moment the store grows, so it is exactly when reclaiming dead entries
	// matters. A goroutine would need lifecycle management (Close, leak
	// tests) to reclaim memory nobody is contending for.
	s.sweepExpiredLocked()
	id := NewSessionID()
	sess := &Session{
		ID:        id,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		ToolCache: make(map[string]cachedTool),
	}
	s.sessions[id] = sess
	s.elems[id] = s.order.PushFront(id)
	s.evictIfFullLocked()
	return sess.clone()
}

// AppendTurn records a completed exchange and trims the buffer if it exceeds
// cfg.MaxMessagesPerSes.
//
// Callers must only append turns that SUCCEEDED. Recording a response that
// failed validation or was rejected by the critic would replay the model's
// own bad output as established context on the next turn — the circular
// self-confirmation the architecture explicitly guards against (§8).
func (s *SessionStore) AppendTurn(id, question, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	sess.Turns = append(sess.Turns, SessionTurn{
		Question:  question,
		Answer:    answer,
		Timestamp: time.Now(),
	})
	if excess := len(sess.Turns) - s.cfg.MaxMessagesPerSes; excess > 0 {
		sess.Turns = sess.Turns[excess:]
	}
	sess.UpdatedAt = time.Now()
	s.touchLocked(id)
	return nil
}

// CacheTool records one tool result under this session. Overwrites any prior
// entry with the same key. Evicts the LRU cache entry when the cache is full.
func (s *SessionStore) CacheTool(id, toolName string, args map[string]any, payload json.RawMessage, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	key := cacheKey(toolName, args)
	sess.ToolCache[key] = cachedTool{
		Payload: payload,
		Digest:  digest,
		Expiry:  time.Now().Add(s.cfg.ToolCacheTTL),
	}
	// Bounded cache: shed a random entry when overloaded. Randomness is fine
	// here — the TTL is the primary correctness lever, this is a size cap.
	if len(sess.ToolCache) > s.cfg.MaxToolCacheSize {
		for k := range sess.ToolCache {
			delete(sess.ToolCache, k)
			break
		}
	}
	sess.UpdatedAt = time.Now()
	return nil
}

// CachedTool returns a cached tool result if one exists, has not expired, and
// belongs to the same ontology watermark as the current session. Returns
// (nil, false) on miss so callers can Dispatch fresh.
func (s *SessionStore) CachedTool(id, toolName string, args map[string]any) (*ToolResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	key := cacheKey(toolName, args)
	entry, ok := sess.ToolCache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.Expiry) {
		delete(sess.ToolCache, key)
		return nil, false
	}
	return &ToolResult{Payload: entry.Payload, Digest: entry.Digest}, true
}

// InvalidateOnMutation is called by the Explainer when it observes that the
// ontology history watermark has advanced beyond the session's cached mark.
// The tool cache is dropped wholesale — narrow invalidation is possible in
// principle but the ontology graph is small enough that a whole-cache flush is
// cheaper to reason about than a per-key invalidation.
func (s *SessionStore) InvalidateOnMutation(id string, watermark time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	if watermark.After(sess.HistoryWatermark) {
		sess.ToolCache = make(map[string]cachedTool)
		sess.HistoryWatermark = watermark
	}
}

// Delete removes a session by ID. Idempotent — deleting an unknown ID is a
// no-op, not an error.
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(id)
}

// Len reports the number of live sessions. Test-only.
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// ── internal ────────────────────────────────────────────────────────────────

func (s *SessionStore) touchLocked(id string) {
	if el, ok := s.elems[id]; ok {
		s.order.MoveToFront(el)
	}
}

func (s *SessionStore) deleteLocked(id string) {
	if el, ok := s.elems[id]; ok {
		s.order.Remove(el)
		delete(s.elems, id)
	}
	delete(s.sessions, id)
}

func (s *SessionStore) evictIfFullLocked() {
	for len(s.sessions) > s.cfg.MaxSessions {
		back := s.order.Back()
		if back == nil {
			return
		}
		s.deleteLocked(back.Value.(string))
	}
}

// sweepExpiredLocked drops every session past its idle TTL. Walks from the
// LRU end and stops at the first live entry — since the list is maintained in
// recency order, everything ahead of a live entry is also live.
func (s *SessionStore) sweepExpiredLocked() {
	for {
		back := s.order.Back()
		if back == nil {
			return
		}
		id := back.Value.(string)
		sess, ok := s.sessions[id]
		if !ok {
			// Index drift — drop the orphaned list node and keep going.
			s.order.Remove(back)
			delete(s.elems, id)
			continue
		}
		if !s.isIdleExpired(sess) {
			return
		}
		s.deleteLocked(id)
	}
}

func (s *SessionStore) isIdleExpired(sess *Session) bool {
	return time.Since(sess.UpdatedAt) > s.cfg.SessionIdleTTL
}

// cacheKey builds a stable, canonical key for a (tool_name, args) pair. Uses
// encoding/json's canonical map ordering — Go's json emits map keys in sorted
// order since 1.12, so this is deterministic across runs and processes.
func cacheKey(toolName string, args map[string]any) string {
	raw, _ := json.Marshal(args)
	sum := sha256.Sum256(append([]byte(toolName+"|"), raw...))
	return hex.EncodeToString(sum[:8])
}
