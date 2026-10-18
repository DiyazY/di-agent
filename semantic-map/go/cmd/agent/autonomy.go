package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/autonomy"
	"github.com/DiyazY/di-agent/pkg/semmap"
)

// autonomyOptions bundles the -autonomy-* flags. Each part is named, and built from
// the autonomy package's registries, so a backend, rule, tool or renderer is
// swapped by a flag rather than by code.
type autonomyOptions struct {
	Level      string // "off" runs no reviewer
	Interval   time.Duration
	PerCycle   int
	Thresholds autonomy.Thresholds
	Rules      string // comma-separated rule names, or "none"
	Renderer   string

	DecisionBackend, DecisionURL, DecisionModel, DecisionKeepAlive, DecisionEffort string
	ReasoningBackend, ReasoningURL, ReasoningModel, ReasoningKeepAlive             string
	ReasoningTools                                                                 string
}

// buildReviewer constructs the candidate reviewer, or nil when -autonomy=off.
// Bearer tokens come from AUTONOMY_DECISION_API_KEY and AUTONOMY_REASONING_API_KEY,
// never from a flag, so they stay out of process listings.
func buildReviewer(sm *semmap.SemanticMap, opt autonomyOptions) (*autonomy.Reviewer, error) {
	if opt.Level == "" || opt.Level == "off" {
		return nil, nil
	}
	level, err := autonomy.ParseLevel(opt.Level)
	if err != nil {
		return nil, err
	}
	cfg, err := autonomy.Build(sm, autonomy.Parts{
		Renderer:        opt.Renderer,
		Rules:           splitList(opt.Rules),
		DecisionBackend: opt.DecisionBackend,
		Decision: autonomy.Spec{URL: opt.DecisionURL, Model: opt.DecisionModel, KeepAlive: opt.DecisionKeepAlive,
			APIKey:  os.Getenv("AUTONOMY_DECISION_API_KEY"),
			Options: map[string]string{"reasoning_effort": opt.DecisionEffort}},
		ReasoningBackend: opt.ReasoningBackend,
		Reasoning: autonomy.Spec{URL: opt.ReasoningURL, Model: opt.ReasoningModel, KeepAlive: opt.ReasoningKeepAlive,
			APIKey: os.Getenv("AUTONOMY_REASONING_API_KEY")},
		Tools: splitList(opt.ReasoningTools),
	})
	if err != nil {
		return nil, err
	}
	cfg.Level, cfg.Interval, cfg.MaxPer, cfg.Thresholds = level, opt.Interval, opt.PerCycle, opt.Thresholds
	return autonomy.New(sm, cfg)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// registerAutonomyRoutes exposes the reviewer's configuration and counts. With no
// reviewer it answers {"level":"off"}, so a reader can tell "off" from "unknown".
func registerAutonomyRoutes(mux *http.ServeMux, r *autonomy.Reviewer) {
	mux.HandleFunc("GET /autonomy", func(w http.ResponseWriter, _ *http.Request) {
		if r == nil {
			writeJSON(w, map[string]string{"level": "off"})
			return
		}
		writeJSON(w, r.Status())
	})
}

// autonomyStatus is what GET /state reports of the reviewer: the level the map is
// being shaped under.
func autonomyStatus(r *autonomy.Reviewer) func() *autonomy.Status {
	return func() *autonomy.Status {
		if r == nil {
			return &autonomy.Status{Level: "off"}
		}
		s := r.Status()
		return &s
	}
}
