// Package client is a tiny HTTP wrapper around the daemon's
// POST /ingest-sample endpoint.
//
// Intentionally minimal: replay only needs to ship MetricSamples; it does
// not need /graph, /history, or any of the mutation endpoints. Keeping
// this client narrow means cmd/replay/ stays decoupled from cmd/mapctl/
// and from internal packages. The DTOs duplicate cmd/agent/dto.go for
// the same wire-boundary reason mapctl/client/types.go does.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MetricSampleRequest mirrors cmd/agent/dto.go MetricSampleRequest. Duplicated
// on the wire boundary so cmd/replay/ does not need to import internal Go
// packages.
type MetricSampleRequest struct {
	NodeID        string            `json:"node_id"`
	MetricType    string            `json:"metric_type"`
	Value         float64           `json:"value"`
	TimestampUnix int64             `json:"timestamp_unix"`
	EventID       string            `json:"event_id"`
	Subject       string            `json:"subject,omitempty"`
	Unit          string            `json:"unit,omitempty"`
	Range         *[2]float64       `json:"range,omitempty"`
	Source        string            `json:"source,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// ErrorResponse mirrors cmd/agent/dto.go ErrorResponse. Decoded from non-2xx
// responses so the replay tool can surface a clear failure message.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Client is a thin HTTP client targeted at one daemon address. Safe for
// concurrent use; reusing one across the whole replay run keeps the
// underlying http.Client's connection pool warm.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New constructs a Client pointing at addr (e.g. "http://localhost:8080").
// Default timeout is 10s — generous enough for /ingest-sample which is a
// few-microsecond synchronous call on the daemon side.
func New(addr string) *Client {
	addr = strings.TrimRight(addr, "/")
	return &Client{
		BaseURL: addr,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// IngestSample posts one MetricSample to /ingest-sample. Returns nil on
// 2xx; on 4xx/5xx it returns an error that includes the daemon's
// {"error":"..."} body when present.
func (c *Client) IngestSample(ctx context.Context, req MetricSampleRequest) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return fmt.Errorf("encode sample: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/ingest-sample", &buf)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var er ErrorResponse
	if json.Unmarshal(body, &er) == nil && er.Error != "" {
		return fmt.Errorf("ingest-sample %d: %s", resp.StatusCode, er.Error)
	}
	return fmt.Errorf("ingest-sample %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
