package agentmain

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/DiyazY/di-agent/pkg/autonomy"
	"github.com/DiyazY/di-agent/pkg/semmap"
	"github.com/DiyazY/di-agent/pkg/spec"
)

// buildReviewer constructs the candidate reviewer from the spec's review block, or
// nil when its level is off. Each part is named, and built from the autonomy
// package's registries, so a backend, rule, tool or renderer is swapped by the
// file rather than by code. Bearer tokens are decision.api_key and
// reasoning.api_key; write them as ${VAR} to keep them out of the file.
func buildReviewer(sm *semmap.SemanticMap, opt spec.Review) (*autonomy.Reviewer, error) {
	if opt.Level == "" || opt.Level == "off" {
		return nil, nil
	}
	level, err := autonomy.ParseLevel(opt.Level)
	if err != nil {
		return nil, err
	}
	cfg, err := autonomy.Build(sm, autonomy.Parts{
		Renderer:        opt.Renderer,
		Rules:           opt.Rules,
		DecisionBackend: opt.Decision.Backend,
		Decision: autonomy.Spec{URL: opt.Decision.URL, Model: opt.Decision.Model, KeepAlive: opt.Decision.KeepAlive,
			APIKey: opt.Decision.APIKey, Timeout: opt.Decision.Timeout,
			Options: map[string]string{"reasoning_effort": opt.Decision.ReasoningEffort}},
		ReasoningBackend: opt.Reasoning.Backend,
		Reasoning: autonomy.Spec{URL: opt.Reasoning.URL, Model: opt.Reasoning.Model, KeepAlive: opt.Reasoning.KeepAlive,
			APIKey: opt.Reasoning.APIKey, Timeout: opt.Reasoning.Timeout},
		Tools: opt.Reasoning.Tools,
	})
	if err != nil {
		return nil, err
	}
	cfg.Level, cfg.Interval, cfg.MaxPer = level, opt.Interval, opt.PerCycle
	cfg.Thresholds = autonomy.Thresholds{Confirm: opt.Thresholds.Confirm, Reject: opt.Thresholds.Reject, Defer: opt.Thresholds.Defer}
	return autonomy.New(sm, cfg)
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

// startReviewLoop reviews candidates in the background: a cycle, then a wait of every
// at the pace, until ctx ends. A nil reviewer (level off) starts nothing.
func startReviewLoop(ctx context.Context, r *autonomy.Reviewer, every time.Duration, p *Pace) {
	if r == nil {
		return
	}
	if every <= 0 {
		every = time.Minute // as autonomy.New defaults it
	}
	log.Printf("autonomy: reviewing candidates every %s at level %s", every, r.Status().Level)
	go func() {
		for {
			r.Cycle(ctx)
			if !p.wait(ctx, every) {
				return
			}
		}
	}()
}
