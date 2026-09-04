// Package client is the wire face of the sample boundary for applications: a
// workload on the node pushes its own metrics to the agent under its own subject,
// with the unit and range it declares. It is the same MetricSample an in-process
// collector produces, arriving over HTTP.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Metric is one type the application declares, with its meaning.
type Metric struct {
	Type  string
	Unit  string
	Range [2]float64
}

// Client pushes samples for one subject on one node.
type Client struct {
	BaseURL string
	NodeID  string
	Subject string
	Source  string
	HTTP    *http.Client
}

// New builds a client. On Kubernetes, nodeID comes from the downward API
// spec.nodeName and subject is "pod:" + metadata.uid, so the application's
// properties land on the same subject the cgroup collector observes.
func New(baseURL, nodeID, subject, source string) *Client {
	return &Client{BaseURL: baseURL, NodeID: nodeID, Subject: subject, Source: source,
		HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// EventID is deterministic over (source, node, subject, metric, timestamp), so a
// retried push is recognised by the map rather than counted twice.
func EventID(source, node, subject, metric string, at time.Time) string {
	h := sha256.Sum256([]byte(source + "|" + node + "|" + subject + "|" + metric + "|" + strconv.FormatInt(at.Unix(), 10)))
	return hex.EncodeToString(h[:8])
}

type sample struct {
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

// Push posts one reading. 202 and 204 are success; anything else is an error
// carrying the server's body.
func (c *Client) Push(ctx context.Context, m Metric, value float64, at time.Time, labels map[string]string) error {
	rng := m.Range
	body, err := json.Marshal(sample{
		NodeID: c.NodeID, MetricType: m.Type, Value: value, TimestampUnix: at.Unix(),
		EventID: EventID(c.Source, c.NodeID, c.Subject, m.Type, at),
		Subject: c.Subject, Unit: m.Unit, Range: &rng, Source: c.Source, Labels: labels,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/ingest-sample", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("ingest-sample: %s: %s", resp.Status, bytes.TrimSpace(msg))
}
