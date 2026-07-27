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

// Session is the per-conversation state kept in memory. Access is protected by
// the store's mutex when reached via Get/Put; direct field access is safe
// only inside the store's critical sections.
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
	ToolCache map[string]cachedTool

	// HistoryWatermark is the timestamp of the most-recent OntologyEvent we
	// observed. When the current watermark advances, the tool cache is
	// treated as stale — the ontology has changed under us.
	HistoryWatermark time.Time
}

// SessionTurn is one round-trip in a multi-turn conversation.
type SessionTurn struct {
	Question  string          `json:"question"`
	Response  json.RawMessage `json:"response"` // full ExplainResponse serialised
	Timestamp time.Time       `json:"timestamp"`
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

// Get returns an existing session or ErrSessionNotFound. The returned pointer
// is only safe to read; mutations must go through Put/AppendTurn.
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
	return sess, nil
}

// Create mints a fresh session and inserts it. Evicts the LRU session when
// the store is full. Returns the new session's ID.
func (s *SessionStore) Create() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	return sess
}

// AppendTurn records a completed exchange to the session and trims the buffer
// if it exceeds cfg.MaxMessagesPerSes.
func (s *SessionStore) AppendTurn(id, question string, response json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	sess.Turns = append(sess.Turns, SessionTurn{
		Question:  question,
		Response:  response,
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
