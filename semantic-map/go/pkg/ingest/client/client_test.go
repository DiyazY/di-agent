package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	err := c.Push(context.Background(), Metric{Type: "queue_depth", Unit: "items", Range: [2]float64{0, 100}}, 7, at,
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

func TestPushReportsServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, 409)
	}))
	defer srv.Close()
	c := New(srv.URL, "node_1", "pod:x", "app")
	if err := c.Push(context.Background(), Metric{Type: "m", Unit: "u", Range: [2]float64{0, 1}}, 0.5, time.Now(), nil); err == nil {
		t.Fatal("a 409 must surface as an error")
	}
}
