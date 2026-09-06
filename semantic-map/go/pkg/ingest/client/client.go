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
	"github.com/DiyazY/di-agent/pkg/types"
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

// Ack is what the agent said about an accepted reading. Routed is true when a
// construct summarises the metric type (204); false when the reading was recorded
// as a property nothing summarises (202), which is always the case for a scoped
// subject and is otherwise how a mistyped metric type shows up — Note carries the
// agent's words for it.
type Ack struct {
	Status int
	Routed bool
	Note   string
}

// validate refuses a sample the agent would refuse, naming the field, before any
// request is made.
func (c *Client) validate(m Metric) error {
	if !types.ValidSubject(c.Subject) {
		return fmt.Errorf("subject %q is not <kind>:<identity> over [A-Za-z0-9._:-]", c.Subject)
	}
	if !types.ValidMetricType(m.Type) {
		return fmt.Errorf("metric type %q is not a single segment over [A-Za-z0-9._-]", m.Type)
	}
	if m.Unit == "" {
		return fmt.Errorf("metric %s declares no unit", m.Type)
	}
	if m.Range[1] <= m.Range[0] {
		return fmt.Errorf("metric %s declares an empty range %v", m.Type, m.Range)
	}
	return nil
}

// Push posts one reading. 202 and 204 are success and the Ack says which; anything
// else is an error carrying the server's body.
func (c *Client) Push(ctx context.Context, m Metric, value float64, at time.Time, labels map[string]string) (Ack, error) {
	if err := c.validate(m); err != nil {
		return Ack{}, err
	}
	rng := m.Range
	body, err := json.Marshal(types.MetricSample{
		NodeID: c.NodeID, MetricType: types.MetricType(m.Type), Value: value, TimestampUnix: at.Unix(),
		EventID: EventID(c.Source, c.NodeID, c.Subject, m.Type, at),
		Subject: c.Subject, Unit: m.Unit, Range: &rng, Source: c.Source, Labels: labels,
	})
	if err != nil {
		return Ack{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/ingest-sample", bytes.NewReader(body))
	if err != nil {
		return Ack{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Ack{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return Ack{Status: resp.StatusCode, Routed: true}, nil
	case http.StatusAccepted:
		var reply struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&reply)
		return Ack{Status: resp.StatusCode, Routed: false, Note: reply.Note}, nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return Ack{Status: resp.StatusCode}, fmt.Errorf("ingest-sample: %s: %s", resp.Status, bytes.TrimSpace(msg))
}
