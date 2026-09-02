package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiyazY/di-agent/cmd/mapctl/client"
)

// TestRoot_ListsAllSubcommands asserts that `mapctl --help` lists every
// subcommand from the inventory. If a subcommand is renamed or removed, this
// test fails — making the inventory load-bearing.
func TestRoot_ListsAllSubcommands(t *testing.T) {
	root := NewRootCmd()
	got := map[string]bool{}
	for _, c := range root.Commands() {
		got[c.Name()] = true
	}
	want := []string{
		"graph", "edges", "history", "strength", "deprecate", "construct",
		"proposition", "reset", "candidates", "recommend", "simulate",
		"watch", "dot", "health", "peers", "version", "completion",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing subcommand %q in root.Commands()", name)
		}
	}
}

// TestRoot_PersistentFlagsParse asserts the global flags are wired and
// retain their values after parsing.
func TestRoot_PersistentFlagsParse(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"--addr", "http://example:9000", "--json", "--no-color", "graph", "--help"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	addr, _ := root.PersistentFlags().GetString("addr")
	if addr != "http://example:9000" {
		t.Errorf("addr: got %q, want http://example:9000", addr)
	}
	jsonFlag, _ := root.PersistentFlags().GetBool("json")
	if !jsonFlag {
		t.Errorf("--json should be true")
	}
	noColor, _ := root.PersistentFlags().GetBool("no-color")
	if !noColor {
		t.Errorf("--no-color should be true")
	}
}

// TestRoot_GraphJSON_AgainstFakeAgent integration-tests the full path:
// httptest agent → cobra → client → render.JSON. Verifies --json yields
// parseable JSON of the expected shape.
func TestRoot_GraphJSON_AgainstFakeAgent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /graph", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.GraphSnapshot{
			Constructs:   []client.ConstructDTO{{ConstructID: "RC", Name: "Resource & Cost"}},
			Propositions: []client.PropositionDTO{{PropositionID: "P3", FromConstruct: "RC", ToConstruct: "PS", Direction: "+", PriorStrength: 0.7}},
			Edges:        []client.EdgeDTO{{FromID: "RC", ToID: "PS", PropositionID: "P3", Direction: "+", Established: f64(0.7), Effective: f64(0.7), Basis: "established", EMAWeight: 0.7}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := NewRootCmd()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(new(bytes.Buffer))
	root.SetContext(context.Background())
	root.SetArgs([]string{"--addr", srv.URL, "--json", "graph"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var snap client.GraphSnapshot
	if err := json.Unmarshal(buf.Bytes(), &snap); err != nil {
		t.Fatalf("output not valid JSON: %v\noutput=%q", err, buf.String())
	}
	if len(snap.Constructs) != 1 || snap.Constructs[0].ConstructID != "RC" {
		t.Errorf("unexpected constructs: %+v", snap.Constructs)
	}
}

// TestRoot_HelpContainsKnownSubcommands runs the help subcommand and verifies
// each expected subcommand appears in the rendered output. This is the
// belt-and-braces alongside TestRoot_ListsAllSubcommands: it catches cases
// where a subcommand is registered but hidden.
func TestRoot_HelpContainsKnownSubcommands(t *testing.T) {
	root := NewRootCmd()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, name := range []string{
		"graph", "edges", "history", "strength", "deprecate", "construct",
		"proposition", "reset", "candidates", "recommend", "simulate",
		"watch", "dot", "health", "peers", "version", "completion",
	} {
		if !strings.Contains(out, name) {
			t.Errorf("--help output missing subcommand %q", name)
		}
	}
}

// f64 returns a pointer to a float literal, for the optional strength fields on
// EdgeDTO where absence and zero mean different things.
func f64(v float64) *float64 { return &v }
