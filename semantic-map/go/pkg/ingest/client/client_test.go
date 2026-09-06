package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPushSendsADeclaredScopedSample(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest-sample" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request shape", 400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(srv.URL, "node_1", "pod:8f3c", "app:transcoder")
	at := time.Unix(1_700_000_000, 0)
	_, err := c.Push(context.Background(), Metric{Type: "queue_depth", Unit: "items", Range: [2]float64{0, 100}}, 7, at,
		map[string]string{"stage": "transcode"})
	if err != nil {
		t.Fatal(err)
	}
	if got["node_id"] != "node_1" || got["subject"] != "pod:8f3c" || got["metric_type"] != "queue_depth" ||
		got["unit"] != "items" || got["value"] != 7.0 || got["source"] != "app:transcoder" {
		t.Errorf("body %v; want node, subject, metric, unit, value and source", got)
	}
	if rng, ok := got["range"].([]any); !ok || rng[1] != 100.0 {
		t.Errorf("range %v; want [0,100]", got["range"])
	}
	if got["event_id"] != EventID("app:transcoder", "node_1", "pod:8f3c", "queue_depth", at) {
		t.Error("event id is not the deterministic one")
	}
}

// TestPushAcceptsARoutedSampleAnswered204: the daemon answers 202 for a sample it
// admitted but could not route and 204 for one it routed into a construct. Both are
// success; a client that accepted only 202 would fail every routed push.
func TestPushAcceptsARoutedSampleAnswered204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL, "node_1", "", "app")
	if _, err := c.Push(context.Background(), Metric{Type: "cpu_utilization", Unit: "fraction", Range: [2]float64{0, 1}}, 0.5, time.Now(), nil); err != nil {
		t.Fatalf("a 204 must be success: %v", err)
	}
}

// TestPushReportsServerErrorsWithTheBody: the body is the only diagnostic an
// application gets, so the error must carry the status and the server's words.
func TestPushReportsServerErrorsWithTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"foreign sample: node_1 is not this agent"}`, 409)
	}))
	defer srv.Close()
	c := New(srv.URL, "node_1", "pod:x", "app")
	_, err := c.Push(context.Background(), Metric{Type: "m", Unit: "u", Range: [2]float64{0, 1}}, 0.5, time.Now(), nil)
	if err == nil {
		t.Fatal("a 409 must surface as an error")
	}
	for _, want := range []string{"409", "foreign sample"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestPushReportsWhetherTheSampleWasRouted: 202 carries a body saying the reading
// was recorded but reached no construct; an application that mistyped a routed
// metric type could never learn that when the body was discarded.
func TestPushReportsWhetherTheSampleWasRouted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "x") {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["metric_type"] == "cpu_utilization" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"recorded":true,"routed":false,"metric_type":"cpu_utilisation","note":"not in the domain specification's routing table"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "node_1", "", "app")
	ack, err := c.Push(context.Background(), Metric{Type: "cpu_utilization", Unit: "fraction", Range: [2]float64{0, 1}}, 0.5, time.Now(), nil)
	if err != nil || !ack.Routed {
		t.Errorf("routed push: ack=%+v err=%v; want Routed true", ack, err)
	}
	ack, err = c.Push(context.Background(), Metric{Type: "cpu_utilisation", Unit: "fraction", Range: [2]float64{0, 1}}, 0.5, time.Now(), nil)
	if err != nil || ack.Routed || !strings.Contains(ack.Note, "routing table") {
		t.Errorf("unrouted push: ack=%+v err=%v; want Routed false with the server's note", ack, err)
	}
}

// TestPushRefusesAnUndeclaredMetricBeforeSending: the server would refuse these
// anyway; refusing them here names the field instead of a 400 the app has to parse.
func TestPushRefusesAnUndeclaredMetricBeforeSending(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	good := Metric{Type: "queue_depth", Unit: "items", Range: [2]float64{0, 100}}
	for _, c := range []struct {
		name   string
		client *Client
		metric Metric
	}{
		{"bad subject", New(srv.URL, "n", "pod/evil", "app"), good},
		{"bad metric type", New(srv.URL, "n", "pod:a", "app"), Metric{Type: "queue@depth", Unit: "items", Range: [2]float64{0, 100}}},
		{"empty unit", New(srv.URL, "n", "pod:a", "app"), Metric{Type: "queue_depth", Range: [2]float64{0, 100}}},
		{"empty range", New(srv.URL, "n", "pod:a", "app"), Metric{Type: "queue_depth", Unit: "items"}},
	} {
		if _, err := c.client.Push(context.Background(), c.metric, 1, time.Now(), nil); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if calls != 0 {
		t.Errorf("%d requests reached the server for samples the client should have refused", calls)
	}
}
