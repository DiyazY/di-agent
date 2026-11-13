package collectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/types"
)

// maxHTTPBody bounds one response body, the same 4 MiB an exec reply line gets. A
// sidecar that sends more is faulted rather than allowed to grow the agent's
// memory without limit.
const maxHTTPBody = 4 << 20

// HTTPCollector reaches a sidecar plugin speaking wire protocol v1 as POST requests
// to one URL, and presents it as a CollectorContract.
//
// There is no connection to lose, so a fault is only remembered: after any
// failure the next Collect sends hello before collect, which is what a restarted
// sidecar needs to hear. Spacing the retries is the supervisor's backoff.
type HTTPCollector struct {
	env     Env
	name    string
	sp      spec.HTTPSpec
	timeout time.Duration
	config  map[string]any
	// client has no client-level timeout; each request is bounded by the context
	// deadline of the Collect or Start that makes it. It follows no redirect: a 3xx
	// is a non-2xx answer and so a fault, rather than a way for the configured URL
	// to send the agent's requests somewhere it was not told about.
	client *http.Client

	// mu serialises Start, Collect and Close, which own seq and needHello.
	mu        sync.Mutex
	needHello bool
	closed    bool
	seq       uint64
	clock     nowClock

	// availMu guards the last successful reply's available list and drops.
	availMu   sync.Mutex
	available []types.MetricType
	health    error
	drops     []contracts.SampleDrop
}

// NewHTTPCollector prepares the sidecar entry name; nothing is sent until Start. A
// non-positive timeout takes the default, min(interval/2, 5s).
func NewHTTPCollector(env Env, name string, sp spec.HTTPSpec, timeout time.Duration, config map[string]any) *HTTPCollector {
	if config == nil {
		config = map[string]any{}
	}
	return &HTTPCollector{env: env, name: name, sp: sp, timeout: resolveTimeout(env, timeout),
		config: config, needHello: true,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Start sends hello. An error means the sidecar was unreachable or refused it; the
// daemon refuses to start on it.
func (h *HTTPCollector) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("collector closed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	return h.hello(ctx)
}

// Collect sends one collect request and returns the stamped samples. After an
// earlier fault it sends hello first; a failing hello is this tick's fault. The
// hello and the collect share one deadline of the configured timeout, because the
// supervisor waits only a short grace beyond it.
func (h *HTTPCollector) Collect() ([]*types.MetricSample, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("collector closed")
	}
	h.availMu.Lock()
	h.drops = nil
	h.availMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	if h.needHello {
		if err := h.hello(ctx); err != nil {
			return nil, err
		}
	}
	h.seq++
	seq := h.seq
	now := h.clock.next(time.Now())
	body, err := h.post(ctx, CollectRequest{Op: "collect", Seq: seq, NowUnix: now})
	if err != nil {
		return nil, h.fault(err)
	}
	r, err := decodeReply(body)
	if err != nil {
		return nil, h.fault(err)
	}
	if err := checkCollect(r, seq); err != nil {
		return nil, h.fault(err)
	}
	samples, avail, drops := stampReply(h.name, h.env.NodeID, now, r)
	h.availMu.Lock()
	h.available = avail
	h.drops = drops
	h.health = replyHealth(r)
	h.availMu.Unlock()
	return samples, nil
}

// Health is the last reply's unhealthy reason, or nil. A doctor an http sidecar
// declares is not run: a network endpoint does not name commands for this node.
func (h *HTTPCollector) Health() error {
	h.availMu.Lock()
	defer h.availMu.Unlock()
	return h.health
}

// TakeDrops returns what the last Collect dropped from the plugin's reply —
// malformed and duplicate samples, invalid available entries — and forgets it.
func (h *HTTPCollector) TakeDrops() []contracts.SampleDrop {
	h.availMu.Lock()
	defer h.availMu.Unlock()
	d := h.drops
	h.drops = nil
	return d
}

// SourceID is the entry name, so it is stable across reconnects.
func (h *HTTPCollector) SourceID() string { return h.name }

// AvailableMetrics is the last successful reply's available list, or the types
// that reply carried.
func (h *HTTPCollector) AvailableMetrics() []types.MetricType {
	h.availMu.Lock()
	defer h.availMu.Unlock()
	return append([]types.MetricType(nil), h.available...)
}

// Close has nothing to release: every request opens and ends within its call.
func (h *HTTPCollector) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.client.CloseIdleConnections()
	return nil
}

// hello performs the handshake and clears needHello on success. Called with mu held.
func (h *HTTPCollector) hello(ctx context.Context) error {
	req := newHello(h.env, h.name, h.config)
	body, err := h.post(ctx, req)
	if err != nil {
		return h.fault(fmt.Errorf("hello: %w", err))
	}
	var r HelloReply
	if err := json.Unmarshal(body, &r); err != nil {
		return h.fault(fmt.Errorf("hello reply is not JSON: %w", err))
	}
	if err := checkHello(r); err != nil {
		return h.fault(err)
	}
	h.needHello = false
	return nil
}

// fault records that the next contact must say hello again, and returns err.
func (h *HTTPCollector) fault(err error) error {
	h.needHello = true
	return err
}

// redactURLError rewrites the URL a *url.Error carries to spec.RedactURL's view,
// so a credential in the configured URL never reaches last_error or the log.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		c := *ue
		c.URL = spec.RedactURL(ue.URL)
		return &c
	}
	return err
}

// post sends one request and returns the response body: a non-2xx status, a
// transport error, a timeout or a body over maxHTTPBody is an error.
func (h *HTTPCollector) post(ctx context.Context, req any) ([]byte, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, h.sp.URL, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("bad url %s", spec.RedactURL(h.sp.URL))
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(hr)
	if err != nil {
		return nil, redactURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("sidecar answered %s", resp.Status)
	}
	// One byte past the cap tells an oversized body from one that is exactly full.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}
	if len(body) > maxHTTPBody {
		return nil, fmt.Errorf("reply exceeds %d MiB", maxHTTPBody>>20)
	}
	return body, nil
}
