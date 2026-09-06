// Package client wraps the di-agent semantic-map HTTP API.
//
// Every method maps to one endpoint. Non-2xx responses decode the
// ErrorResponse body when present and return its Error field; otherwise the
// status line. The client uses net/http directly — no global state, no
// retries, no streaming. Callers control timeouts via http.Client.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client is a lightweight wrapper around an HTTP client targeted at a
// single daemon address. Construct via New and reuse — it is safe for
// concurrent use.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client pointing at addr (e.g. "http://localhost:8080") with
// a 10s default timeout. Caller may override the embedded HTTP client.
func New(addr string) *Client {
	addr = strings.TrimRight(addr, "/")
	return &Client{
		BaseURL: addr,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// errorFromResponse extracts a useful error from a non-2xx response,
// preferring the JSON {"error":"..."} body shape used by new endpoints
// but falling back to the raw body or status line.
func errorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	if len(body) > 0 {
		var er ErrorResponse
		if json.Unmarshal(body, &er) == nil && er.Error != "" {
			return fmt.Errorf("agent error %d: %s", resp.StatusCode, er.Error)
		}
		return fmt.Errorf("agent error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("agent error %d: %s", resp.StatusCode, resp.Status)
}

// getJSON issues a GET against path (with optional query params) and decodes
// the JSON response into out.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFromResponse(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postJSON issues a POST with body marshaled as JSON. If out is non-nil and
// the response has a body, it is decoded into out. 204 responses skip
// decoding.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFromResponse(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── Read endpoints ────────────────────────────────────────────────────────────

// Graph returns the full graph snapshot (GET /graph).
func (c *Client) Graph(ctx context.Context) (*GraphSnapshot, error) {
	var snap GraphSnapshot
	if err := c.getJSON(ctx, "/graph", nil, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// Edges returns edges, optionally filtered by from/to (GET /edges).
func (c *Client) Edges(ctx context.Context, from, to string) ([]EdgeDTO, error) {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	var edges []EdgeDTO
	if err := c.getJSON(ctx, "/edges", q, &edges); err != nil {
		return nil, err
	}
	return edges, nil
}

// Constructs returns every construct (GET /constructs).
func (c *Client) Constructs(ctx context.Context) ([]ConstructDTO, error) {
	var out []ConstructDTO
	if err := c.getJSON(ctx, "/constructs", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Propositions returns every proposition, including deprecated ones (GET /propositions).
func (c *Client) Propositions(ctx context.Context) ([]PropositionDTO, error) {
	var out []PropositionDTO
	if err := c.getJSON(ctx, "/propositions", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// History returns ontology events since the given string. The since string is
// passed through to the daemon, which accepts empty / RFC3339 / Go duration.
func (c *Client) History(ctx context.Context, since string) ([]OntologyEventDTO, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	var out []OntologyEventDTO
	if err := c.getJSON(ctx, "/history", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Neighbors returns target construct IDs reachable from node (GET /neighbors).
func (c *Client) Neighbors(ctx context.Context, node string) ([]string, error) {
	q := url.Values{"node": []string{node}}
	var out []string
	if err := c.getJSON(ctx, "/neighbors", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Health returns true if the daemon's /healthz responds OK.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var h HealthResponse
	if err := c.getJSON(ctx, "/healthz", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Version returns the daemon's reported version (GET /version).
func (c *Client) Version(ctx context.Context) (*VersionResponse, error) {
	var v VersionResponse
	if err := c.getJSON(ctx, "/version", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Candidates returns pending candidate edges (GET /candidates).
func (c *Client) Candidates(ctx context.Context) ([]CandidateEdge, error) {
	var out []CandidateEdge
	if err := c.getJSON(ctx, "/candidates", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Estimate calls GET /state/estimate with optional hypotheses. assume is
// property → value; without lists subjects or properties taken to their floor.
func (c *Client) Estimate(ctx context.Context, target string, assume map[string]float64, without []string) (*EstimateResponse, error) {
	q := url.Values{}
	q.Set("target", target)
	keys := make([]string, 0, len(assume))
	for k := range assume {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q.Add("assume", k+"="+strconv.FormatFloat(assume[k], 'g', -1, 64))
	}
	for _, w := range without {
		q.Add("without", w)
	}
	var out EstimateResponse
	if err := c.getJSON(ctx, "/state/estimate", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Mutation endpoints ────────────────────────────────────────────────────────

// SetStrength updates a proposition's prior strength.
func (c *Client) SetStrength(ctx context.Context, propID string, strength float64) error {
	return c.postJSON(ctx, "/ontology/strength",
		SetStrengthRequest{PropositionID: propID, Strength: strength}, nil)
}

// Deprecate flags a proposition with a reason.
func (c *Client) Deprecate(ctx context.Context, propID, reason string) error {
	return c.postJSON(ctx, "/ontology/deprecate",
		DeprecateRequest{PropositionID: propID, Reason: reason}, nil)
}

// AddConstruct registers a new construct.
func (c *Client) AddConstruct(ctx context.Context, id, name, description string) error {
	return c.postJSON(ctx, "/ontology/construct",
		AddConstructRequest{ConstructID: id, Name: name, Description: description}, nil)
}

// AddProposition registers a new validated proposition. Direction is "+" / "-".
func (c *Client) AddProposition(ctx context.Context, id, from, to, direction string, prior float64) error {
	return c.postJSON(ctx, "/ontology/proposition",
		AddPropositionRequest{
			PropositionID: id, From: from, To: to,
			Direction: direction, PriorStrength: prior,
		}, nil)
}

// ResetEdge restores the prior weight for an edge between from and to.
func (c *Client) ResetEdge(ctx context.Context, from, to string) error {
	return c.postJSON(ctx, "/agent/reset",
		ResetRequest{From: from, To: to}, nil)
}

// ConfirmCandidate confirms a candidate edge (POST /candidates/{id}/confirm).
func (c *Client) ConfirmCandidate(ctx context.Context, id string) error {
	return c.postJSON(ctx, "/candidates/"+url.PathEscape(id)+"/confirm", nil, nil)
}

// RejectCandidate rejects a candidate edge.
func (c *Client) RejectCandidate(ctx context.Context, id string) error {
	return c.postJSON(ctx, "/candidates/"+url.PathEscape(id)+"/reject", nil, nil)
}

// DeferCandidate defers a candidate edge.
func (c *Client) DeferCandidate(ctx context.Context, id string) error {
	return c.postJSON(ctx, "/candidates/"+url.PathEscape(id)+"/defer", nil, nil)
}

// Recommend issues POST /recommend with the given offload context.
func (c *Client) Recommend(ctx context.Context, octx OffloadContext) (*PeerRecommendation, error) {
	var out PeerRecommendation
	if err := c.postJSON(ctx, "/recommend", octx, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Simulate issues POST /simulate with the given offload context and target.
func (c *Client) Simulate(ctx context.Context, octx OffloadContext, target string) (*OutcomeSimulation, error) {
	var out OutcomeSimulation
	if err := c.postJSON(ctx, "/simulate", SimulateRequest{
		Context: octx, TargetNodeID: target,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Peer coordination endpoints ───────────────────────────────────────────────

// ListPeers returns every registered peer (GET /peers).
func (c *Client) ListPeers(ctx context.Context) ([]PeerDTO, error) {
	var out []PeerDTO
	if err := c.getJSON(ctx, "/peers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddPeer registers a new peer at url with an optional human-readable note
// (POST /peers). Returns the resulting PeerDTO including the derived ID.
func (c *Client) AddPeer(ctx context.Context, url, note string) (*PeerDTO, error) {
	var out PeerDTO
	if err := c.postJSON(ctx, "/peers", AddPeerRequest{URL: url, Note: note}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemovePeer unregisters the peer with the given ID (DELETE /peers/{id}).
func (c *Client) RemovePeer(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.BaseURL+"/peers/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFromResponse(resp)
	}
	return nil
}

// SetPeerTrust manually overrides a peer's trust score (POST /peers/{id}/trust).
func (c *Client) SetPeerTrust(ctx context.Context, id string, value float64) error {
	return c.postJSON(ctx, "/peers/"+url.PathEscape(id)+"/trust",
		SetTrustRequest{Value: value}, nil)
}

// ── Operator tuning ───────────────────────────────────────────────────────────

// Tune sends a natural-language intent string to the daemon and returns
// the applied proposition adjustments.
func (c *Client) Tune(ctx context.Context, intent, operator string) (*TuneResponse, error) {
	body := TuneRequest{Intent: intent, Operator: operator}
	var resp TuneResponse
	if err := c.postJSON(ctx, "/agent/tune", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
