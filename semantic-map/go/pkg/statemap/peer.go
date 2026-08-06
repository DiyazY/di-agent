package statemap

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Peer state is what makes a decentralised claim more than a slogan. Every node runs
// an agent that models its own system; a cluster-level question is then answered by
// asking peers, which requires that knowledge — not just a cost number — can cross a
// node boundary.
//
// The governing rule is that a peer's state is NEVER merged into the local model.
// Merging would produce a map whose properties belong to no definite system, and every
// claim the local model makes about "this system" would quietly become a claim about
// some union of systems. The whole reason confidence means anything here is that its
// subject is a single machine.
//
// So peer state is held separately, labelled with whose it is and when it was
// collected, and every read of it reports its age. A caller combining local and remote
// state does so explicitly, in the open, and can see what it is combining.

// PeerState is one snapshot of another agent's map, as received.
type PeerState struct {
	// PeerID is the identity the peer reported for itself, not the URL it was reached
	// at. A URL is where a peer was found; the identity is what it claims to model,
	// and the two can disagree behind a proxy or after a re-address.
	PeerID string `json:"peer_id"`
	URL    string `json:"url"`

	// FetchedAt and Revision place the snapshot in time on both clocks: ours, so its
	// age can be judged, and theirs, so two snapshots from the same peer can be
	// ordered without trusting clock agreement.
	FetchedAt time.Time `json:"fetched_at"`
	Revision  uint64    `json:"revision"`

	Properties    []Property     `json:"properties"`
	Relationships []Relationship `json:"relationships"`
	Counts        StateCounts    `json:"counts"`

	// Err records why the last attempt failed, if it did. A peer that cannot be
	// reached is a fact about the cluster worth reporting rather than an absence to
	// paper over — silence and "unreachable" are different answers.
	Err string `json:"error,omitempty"`
}

// Age is how long ago this snapshot was taken.
func (p PeerState) Age(now time.Time) time.Duration { return now.Sub(p.FetchedAt) }

// PeerStateFrom builds a snapshot from a view received over the wire.
//
// The identity comes from the view's Owner — what the peer says it models — and not
// from the URL it was fetched from, so a peer reachable at two addresses is one peer
// and a proxy in front of several is not mistaken for one.
func PeerStateFrom(url string, v StateView, fetchedAt time.Time) PeerState {
	return PeerState{
		PeerID:        v.Owner,
		URL:           url,
		FetchedAt:     fetchedAt,
		Revision:      v.Revision,
		Properties:    v.Properties,
		Relationships: v.Relationships,
		Counts:        v.Counts,
	}
}

// PeerView is the cluster-level answer: this node's own state alongside what it last
// heard from each peer, with ages attached and nothing merged.
type PeerView struct {
	Self       StateView            `json:"self"`
	SelfID     string               `json:"self_id"`
	Peers      map[string]PeerState `json:"peers"`
	AsOf       time.Time            `json:"as_of"`
	StaleAfter time.Duration        `json:"stale_after"`

	// Unreachable lists peers whose last fetch failed, so a caller reading a thin
	// cluster view can tell an empty cluster from a partitioned one.
	Unreachable []string `json:"unreachable,omitempty"`

	// Silent maps a URL to the reason it has never answered. These are peers this
	// agent has no state for at all — a wrong address, an agent that never started —
	// and they are reported separately because "never identified itself" is a
	// different situation from "known peer, currently unreachable".
	Silent map[string]string `json:"silent,omitempty"`
}

// PeerStore holds what this agent has heard from other agents. It is deliberately not
// part of Map: the map models one system, and mixing the two types would make it
// possible to answer a question about "this system" with a peer's property by
// accident.
type PeerStore struct {
	mu     sync.RWMutex
	states map[string]PeerState
	selfID string

	// silent holds URLs that have never identified themselves, with the last reason.
	// They are kept out of states because states is keyed by owner, and a peer that
	// never answered has no owner to key it under — inventing one from the URL would
	// put a fictional node in every cluster view.
	silent map[string]string

	// staleAfter is how old a peer snapshot may be before a reader should treat it as
	// history. It does not delete anything: a stale snapshot is still the last thing
	// this agent knew about that peer, which is exactly what it needs during a
	// partition.
	staleAfter time.Duration
	now        func() time.Time
}

// NewPeerStore builds an empty store. staleAfter <= 0 defaults to one minute.
func NewPeerStore(selfID string, staleAfter time.Duration) *PeerStore {
	if staleAfter <= 0 {
		staleAfter = time.Minute
	}
	return &PeerStore{
		states:     make(map[string]PeerState),
		silent:     make(map[string]string),
		selfID:     selfID,
		staleAfter: staleAfter,
		now:        time.Now,
	}
}

// SetClock overrides the store's clock. For tests.
func (s *PeerStore) SetClock(f func() time.Time) {
	s.mu.Lock()
	s.now = f
	s.mu.Unlock()
}

// Record stores a snapshot received from a peer.
//
// A snapshot claiming to come from this agent is rejected. Accepting it would let a
// misconfigured peer — or a loop through a proxy back to this node — install this
// agent's own state as a remote observation, and every subsequent cluster view would
// double-count one machine.
func (s *PeerStore) Record(st PeerState) error {
	if st.PeerID == "" {
		return fmt.Errorf("state fetched from %s names no owner, so it cannot be attributed "+
			"to a system: the peer agent is running without a node identity", st.URL)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selfID != "" && st.PeerID == s.selfID {
		return fmt.Errorf("peer state claims to be from %q, which is this agent: refusing "+
			"to hold our own state as a peer's", st.PeerID)
	}
	if st.FetchedAt.IsZero() {
		st.FetchedAt = s.now()
	}
	// A peer that has just answered is not unreachable and not silent. Clearing both
	// here rather than leaving them to expire means the cluster view describes the
	// present rather than the worst thing that ever happened to this address.
	st.Err = ""
	delete(s.silent, st.URL)
	s.states[st.PeerID] = st
	return nil
}

// RecordFailure notes that a peer could not be reached, keeping whatever was last
// known about it. The distinction between "said nothing" and "could not be asked" is
// the difference between a quiet cluster and a broken one.
//
// It is keyed by URL because a URL is all a failed fetch yields: the identity comes
// from a successful response. A URL already matched to an owner marks that peer
// unreachable and keeps its last snapshot, which is what an agent needs during a
// partition. A URL never yet identified is recorded as silent instead — there is no
// state to preserve and no node to attribute it to.
func (s *PeerStore) RecordFailure(url string, cause error) {
	reason := "unreachable"
	if cause != nil {
		reason = cause.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, st := range s.states {
		if st.URL == url {
			st.Err = reason
			s.states[id] = st
			return
		}
	}
	s.silent[url] = reason
}

// Peer returns what was last heard from one peer.
func (s *PeerStore) Peer(peerID string) (PeerState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.states[peerID]
	return st, ok
}

// View assembles the cluster-level answer: local state, peer snapshots with their
// ages, and the peers that could not be reached — none of it merged.
func (s *PeerStore) View(self StateView) PeerView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	v := PeerView{
		Self:       self,
		SelfID:     s.selfID,
		Peers:      make(map[string]PeerState, len(s.states)),
		AsOf:       s.now(),
		StaleAfter: s.staleAfter,
	}
	for id, st := range s.states {
		v.Peers[id] = st
		if st.Err != "" {
			v.Unreachable = append(v.Unreachable, id)
		}
	}
	sort.Strings(v.Unreachable)
	if len(s.silent) > 0 {
		v.Silent = make(map[string]string, len(s.silent))
		for url, reason := range s.silent {
			v.Silent[url] = reason
		}
	}
	return v
}

// FindProperty answers "who else has this property, and what do they say it is".
//
// This is the query a cluster-level question reduces to, and it returns per-node
// answers rather than an aggregate. An average across nodes would be a number
// describing no machine — the same error as merging, one step later.
func (s *PeerStore) FindProperty(id string) map[string]Property {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]Property{}
	for peerID, st := range s.states {
		for _, p := range st.Properties {
			if p.ID == id {
				out[peerID] = p
				break
			}
		}
	}
	return out
}

// StaleView reports which peer snapshots are older than the staleness window, so a
// caller can decide for itself whether to act on them.
func (s *PeerStore) StaleView() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	var out []string
	for id, st := range s.states {
		if st.FetchedAt.IsZero() || now.Sub(st.FetchedAt) > s.staleAfter {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
