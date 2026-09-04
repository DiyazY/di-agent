package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEstimateCommandRendersProjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state/estimate" || r.URL.Query().Get("target") != "cpu_pressure_ratio" ||
			r.URL.Query().Get("assume") != "queue@pod:a=100" || r.URL.Query().Get("without") != "pod:b" {
			http.Error(w, "unexpected query "+r.URL.RawQuery, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"decision_id":"est-1","revision":9,
		  "answer":{"target":"cpu_pressure_ratio","level":0.3,"confidence":0.5,"status":"active","sensitivity":0.8,"contributions":0.4},
		  "influences":[{"relationship":"queue@pod:a->cpu_pressure_ratio:discovered","source":"queue@pod:a","source_value":50,
		    "effective_strength":0.8,"sign":1,"contribution":0.4,"provenance":"discovered","basis":"asserted","known":true,
		    "hypothetical_source_value":100,"hypothetical_contribution":0.8}],
		  "assumptions":{"queue@pod:a":100},"excluded":["pod:b"],
		  "hypothetical":{"contributions":0.8,"delta":0.4,"projected_level":0.7},
		  "rationale":"r","caveats":["strength is a correlation magnitude used as a unit sensitivity, not a fitted slope"]}`))
	}))
	defer srv.Close()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--addr", srv.URL, "estimate", "cpu_pressure_ratio", "--assume", "queue@pod:a=100", "--without", "pod:b"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"projected", "0.7", "queue@pod:a", "not a fitted slope", "est-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
