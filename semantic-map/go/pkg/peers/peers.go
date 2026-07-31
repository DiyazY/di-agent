// Package peers provides the multi-agent coordination layer for di-agent.
//
// A Registry holds Descriptors for known remote agents, each tagged with a
// trust score in [0,1] that the reasoner uses to weight offload candidates.
// A Client speaks HTTP to remote agents (their /cost, /healthz, /offload
// endpoints) so RecommendPeer can rank live peers by trust-weighted savings.
//
// Design choice (v1): Registry and Client are concrete types, not contract
// interfaces. The contract surface stays at five (Storage, Ontology, Updater,
// Reasoner, Proposer, Collector). When a second implementation arrives — e.g.
// SQLite-backed peer state for richer profiles, or a gossip-based registry —
// we promote the surface to pkg/contracts at that point.
//
// Security stance (v1): no auth on /peers or /offload endpoints. This package
// targets a localhost/lab-network deployment. Production hardening (mTLS,
// signed identities, bearer tokens) is a P7 concern.
package peers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Descriptor captures one peer's identity, address, and current trust state.
//
// ID is derived as sha256(url)[:12] when callers do not supply one, so two
// processes registering the same URL converge on the same identity across
// restarts. The 12-character prefix is enough to disambiguate at the lab scale
// while staying readable on terminal output.
//
// Trust is in [0, 1]. New peers start at 0.5 (no history; we neither extend
// blanket trust nor pre-distrust the operator's chosen address). The reasoner
// filters peers below the configured minimum and weights savings by trust
// when ranking candidates.
//
// LastSeen is updated by the reasoner whenever a successful HTTP probe lands;
// it remains zero until first contact.
type Descriptor struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Trust     float64   `json:"trust"`
	NObserved int       `json:"n_observed"`
	LastSeen  time.Time `json:"last_seen"`
	Note      string    `json:"note,omitempty"`
}

// clone returns a defensive deep copy so callers can mutate the result
// without affecting registry state.
func (d *Descriptor) clone() *Descriptor {
	if d == nil {
		return nil
	}
	out := *d
	return &out
}

// ── Registry ──────────────────────────────────────────────────────────────────

// Registry is the in-memory peer table. Concurrent-safe via sync.RWMutex.
//
// All mutations clamp Trust to [0, 1]. List and Get* return defensive copies
// so callers cannot mutate stored Descriptors directly. Add deduplicates by
// URL — calling Add twice with the same URL returns the existing descriptor
// without disturbing its trust history.
type Registry struct {
	mu      sync.RWMutex
	byID    map[string]*Descriptor
	urlToID map[string]string
}

// NewRegistry returns an empty in-memory peer registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:    make(map[string]*Descriptor),
		urlToID: make(map[string]string),
	}
}

// Add registers a peer at url with an optional human-readable note. The note
// is purely informational (operator label). When url is already registered,
// Add is a no-op that returns the existing descriptor — the trust history is
// preserved so an operator re-issuing an Add does not silently reset trust.
//
// Returns ErrEmptyURL when url is the empty string after trimming.
func (r *Registry) Add(url, note string) (*Descriptor, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, ErrEmptyURL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.urlToID[url]; ok {
		return r.byID[id].clone(), nil
	}
	id := deriveID(url)
	d := &Descriptor{
		ID:    id,
		URL:   url,
		Trust: 0.5, // default at registration — no history yet
		Note:  note,
	}
	r.byID[id] = d
	r.urlToID[url] = id
	return d.clone(), nil
}

// Remove deletes the peer with the given ID. Returns ErrUnknownPeer when no
// such ID is registered. Successful removal is idempotent at the API level —
// callers can re-issue Remove to confirm absence, but the second call returns
// ErrUnknownPeer.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byID[id]
	if !ok {
		return ErrUnknownPeer
	}
	delete(r.byID, id)
	delete(r.urlToID, d.URL)
	return nil
}

// Get returns a copy of the descriptor with the given ID, or (nil, nil) when
// the ID is not registered. The nil-on-miss convention matches StorageContract.
func (r *Registry) Get(id string) (*Descriptor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	if !ok {
		return nil, nil
	}
	return d.clone(), nil
}

// GetByURL returns a copy of the descriptor registered for url, or (nil, nil)
// when url is not known.
func (r *Registry) GetByURL(url string) (*Descriptor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.urlToID[url]
	if !ok {
		return nil, nil
	}
	return r.byID[id].clone(), nil
}

// List returns every registered peer as defensive copies. The slice is sorted
// by ID for stable iteration.
func (r *Registry) List() ([]*Descriptor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Descriptor, 0, len(r.byID))
	// Two-step copy: collect IDs, sort, then clone in order.
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		out = append(out, r.byID[id].clone())
	}
	return out, nil
}

// UpdateTrust applies delta to the peer's trust score (clamped to [0, 1]) and
// increments NObserved. Use a positive delta for a successful interaction and
// a negative one for a failure or rejection.
func (r *Registry) UpdateTrust(id string, delta float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byID[id]
	if !ok {
		return ErrUnknownPeer
	}
	d.Trust = clamp01(d.Trust + delta)
	d.NObserved++
	return nil
}

// SetTrust overrides the peer's trust score directly (clamped to [0, 1]).
// Intended for operator console adjustments and deterministic test setups.
// NObserved is incremented so /peers responses reflect that the operator
// touched this peer.
func (r *Registry) SetTrust(id string, value float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byID[id]
	if !ok {
		return ErrUnknownPeer
	}
	d.Trust = clamp01(value)
	d.NObserved++
	return nil
}

// MarkSeen records that a successful interaction with the peer happened at
// the given timestamp. Use it after any 2xx response from the peer.
func (r *Registry) MarkSeen(id string, when time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byID[id]
	if !ok {
		return ErrUnknownPeer
	}
	d.LastSeen = when
	return nil
}

// ── Client — outbound HTTP to peers ───────────────────────────────────────────

// Client makes HTTP calls to remote agents. It is intentionally tiny: one
// http.Client, a default timeout, and three methods that mirror the daemon's
// /cost, /healthz, and /offload endpoints.
type Client struct {
	http    *http.Client
	Timeout time.Duration
}

// NewClient returns a Client with the given per-request timeout. Pass 0 to
// disable the timeout (not recommended in production).
func NewClient(timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		Timeout: timeout,
	}
}

// ActionCost mirrors the daemon's /cost response. Defined locally so this
// package stays independent of cmd/agent — peers.Client only knows the wire
// format, not the server internals.
type ActionCost struct {
	CPUCost         float64  `json:"CPUCost"`
	ResourceCost    float64  `json:"ResourceCost"`
	EnergyCost      float64  `json:"EnergyCost"` // placeholder: zero until EnergyJoules observations are available
	LatencyEstimate float64  `json:"LatencyEstimate"`
	Confidence      float64  `json:"Confidence"`
	Rationale       string   `json:"Rationale"`
	GraphPathUsed   []string `json:"GraphPathUsed"`
}

// OffloadRequest is the body sent to a peer's /offload endpoint.
type OffloadRequest struct {
	TaskType           string   `json:"task_type"`
	SourceNodeID       string   `json:"source_node_id"`
	DataSizeBytes      int64    `json:"data_size_bytes"`
	LatencyBudgetMs    float64  `json:"latency_budget_ms"`
	EnergyBudgetJoules *float64 `json:"energy_budget_joules,omitempty"`
}

// OffloadResponse is the body returned by a peer's /offload endpoint.
// Accepted indicates the peer believes it can execute within the requested
// budgets. ExpectedLatency / ExpectedResourceCost report the peer's own cost
// estimate so the source agent can record the outcome (and adjust trust).
type OffloadResponse struct {
	Accepted             bool    `json:"accepted"`
	Reason               string  `json:"reason,omitempty"`
	ExpectedLatency      float64 `json:"expected_latency"`
	ExpectedResourceCost float64 `json:"expected_resource_cost"`
	ExpectedEnergy       float64 `json:"expected_energy"` // placeholder: zero until EnergyJoules observations are available
}

// Cost asks the peer at peerURL what it would cost to run taskType for
// sourceNodeID. Returns the peer's ActionCost on 2xx; otherwise wraps the
// HTTP status, transport error, or JSON decode failure.
func (c *Client) Cost(ctx context.Context, peerURL, taskType, sourceNodeID string) (*ActionCost, error) {
	u := strings.TrimRight(peerURL, "/") + "/cost?task=" + escapeQuery(taskType) + "&node=" + escapeQuery(sourceNodeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("peers.Client.Cost: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peers.Client.Cost: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("peers.Client.Cost: peer %s returned %d: %s", peerURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out ActionCost
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("peers.Client.Cost: decode: %w", err)
	}
	return &out, nil
}

// State fetches the peer's semantic map — what properties it exhibits, how they
// relate, and how much of that rests on observation.
//
// This is what makes "cluster-level questions go to peers" mean more than asking for
// one number. Before this, an agent could ask a peer what an action would cost and
// nothing else: not which properties the peer has, not which of them have gone quiet,
// not what it believes about a relation. Those are the questions a node needs answered
// to reason about anything beyond itself.
//
// The returned snapshot is stamped with the fetch time and the peer's own identity, and
// it is the caller's job to keep it apart from local state. The wire type is
// statemap.PeerState rather than a local copy of it, because duplicating the property
// and relationship structs here would give the two ends of the exchange separate
// definitions of the same thing — the shape they agree on is the whole point.
func (c *Client) State(ctx context.Context, peerURL string) (*statemap.PeerState, error) {
	u := strings.TrimRight(peerURL, "/") + "/state"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("peers.Client.State: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peers.Client.State: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("peers.Client.State: peer %s returned %d: %s",
			peerURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var view statemap.StateView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		return nil, fmt.Errorf("peers.Client.State: decode: %w", err)
	}
	if view.Owner == "" {
		// Refused here rather than stored and puzzled over later: unattributed state is
		// the one kind that could quietly be read as someone else's.
		return nil, fmt.Errorf("peers.Client.State: peer %s returned state naming no owner: "+
			"that agent is running without a node identity, so its properties cannot be "+
			"attributed to a system", peerURL)
	}
	st := statemap.PeerStateFrom(peerURL, view, time.Now())
	return &st, nil
}

// Health probes the peer's /healthz endpoint. Returns true only on 2xx —
// transport errors, non-2xx status, and decode failures all count as "down".
func (c *Client) Health(ctx context.Context, peerURL string) bool {
	u := strings.TrimRight(peerURL, "/") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Offload posts an OffloadRequest to the peer and returns its response.
// Used by the trust-update path: a 2xx with Accepted=true is the signal to
// nudge the peer's trust upward; a 2xx with Accepted=false (peer's own
// budget-violation reason) is informational and leaves trust unchanged.
func (c *Client) Offload(ctx context.Context, peerURL string, req *OffloadRequest) (*OffloadResponse, error) {
	if req == nil {
		return nil, errors.New("peers.Client.Offload: nil request")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("peers.Client.Offload: marshal: %w", err)
	}
	u := strings.TrimRight(peerURL, "/") + "/offload"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("peers.Client.Offload: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("peers.Client.Offload: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("peers.Client.Offload: peer %s returned %d: %s", peerURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out OffloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("peers.Client.Offload: decode: %w", err)
	}
	return &out, nil
}

// ── sentinel errors ──────────────────────────────────────────────────────────

var (
	// ErrEmptyURL is returned by Add when the supplied URL trims to "".
	ErrEmptyURL = errors.New("peers: url must be non-empty")
	// ErrUnknownPeer is returned by mutation methods when the ID is unknown.
	ErrUnknownPeer = errors.New("peers: unknown peer ID")
)

// ── helpers ──────────────────────────────────────────────────────────────────

// deriveID hashes url with SHA-256 and returns the first 12 hex characters.
// Stable across restarts and across processes that register the same URL.
func deriveID(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:6]) // 12 hex chars
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// escapeQuery is a tiny URL-encoder for the two query parameter values we
// emit in Cost. We avoid net/url here to keep this package's import surface
// minimal — task and node IDs are alphanumeric in practice.
func escapeQuery(s string) string {
	// Use a real encoder so any future weirdness (spaces, ampersands) is
	// handled correctly. net/url is already in the standard library and
	// already imported elsewhere in this binary; this is the responsible
	// choice over hand-rolled escaping.
	return queryEscape(s)
}

// queryEscape mirrors url.QueryEscape but is inlined here to keep the import
// surface visible and the wire shape obvious.
func queryEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}

// sortStrings is an in-place ascending sort. Avoids importing sort just for
// the registry — keeps the package's go imports tight.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
