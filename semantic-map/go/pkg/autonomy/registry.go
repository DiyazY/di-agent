package autonomy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Every part of the reviewer a deployment may want to swap is registered by name:
// the decision backend, the reasoning backend, the rules, the reasoning tools and the
// evidence renderer. The daemon picks them by flag; a fork adds its own with a
// Register call in an init function, and nothing else in the package changes.

// Spec configures one model backend.
type Spec struct {
	URL       string
	Model     string
	APIKey    string // bearer token, for a backend that wants one; never logged or recorded
	Timeout   time.Duration
	KeepAlive string            // Ollama: how long the model stays resident after a call
	Options   map[string]string // backend-specific settings
}

// DeciderFactory builds a tier 1 backend.
type DeciderFactory func(Spec, Renderer) (Decider, error)

// ReasonerFactory builds a tier 2 backend. tools are the read-only tools it may call.
type ReasonerFactory func(Spec, Renderer, []Tool) (Reasoner, error)

// RuleFactory builds a tier 0 rule.
type RuleFactory func() Rule

// ToolFactory builds a reasoning tool over the map.
type ToolFactory func(Map) Tool

// RendererFactory builds an evidence renderer.
type RendererFactory func() Renderer

type registry[F any] struct {
	mu sync.RWMutex
	m  map[string]F
}

func (r *registry[F]) register(kind, name string, f F) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]F{}
	}
	if _, dup := r.m[name]; dup {
		panic(fmt.Sprintf("autonomy: %s %q registered twice", kind, name))
	}
	r.m[name] = f
}

func (r *registry[F]) get(kind, name string) (F, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.m[name]
	if !ok {
		var zero F
		return zero, fmt.Errorf("no %s named %q (have %s)", kind, name, strings.Join(r.namesLocked(), ", "))
	}
	return f, nil
}

func (r *registry[F]) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *registry[F]) namesLocked() []string {
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var (
	deciders  registry[DeciderFactory]
	reasoners registry[ReasonerFactory]
	rulesReg  registry[RuleFactory]
	toolsReg  registry[ToolFactory]
	renderers registry[RendererFactory]
)

func RegisterDecider(name string, f DeciderFactory) { deciders.register("decision backend", name, f) }
func RegisterReasoner(name string, f ReasonerFactory) {
	reasoners.register("reasoning backend", name, f)
}
func RegisterRule(name string, f RuleFactory)         { rulesReg.register("rule", name, f) }
func RegisterTool(name string, f ToolFactory)         { toolsReg.register("tool", name, f) }
func RegisterRenderer(name string, f RendererFactory) { renderers.register("renderer", name, f) }
func DeciderNames() []string                          { return deciders.names() }
func ReasonerNames() []string                         { return reasoners.names() }
func RuleNames() []string                             { return rulesReg.names() }
func ToolNames() []string                             { return toolsReg.names() }
func RendererNames() []string                         { return renderers.names() }

// Parts names what to build. "none" (or empty) for a backend leaves that tier out.
type Parts struct {
	Renderer string
	Rules    []string

	DecisionBackend string
	Decision        Spec

	ReasoningBackend string
	Reasoning        Spec
	Tools            []string
}

// Build assembles the named parts into a Config, leaving the level, interval and
// thresholds to the caller.
func Build(m Map, p Parts) (Config, error) {
	var cfg Config
	name := p.Renderer
	if name == "" {
		name = "text"
	}
	rf, err := renderers.get("renderer", name)
	if err != nil {
		return cfg, err
	}
	render := rf()
	for _, n := range p.Rules {
		if n == "" || n == "none" {
			continue
		}
		f, err := rulesReg.get("rule", n)
		if err != nil {
			return cfg, err
		}
		cfg.Rules = append(cfg.Rules, f())
	}
	if on(p.DecisionBackend) {
		f, err := deciders.get("decision backend", p.DecisionBackend)
		if err != nil {
			return cfg, err
		}
		if cfg.Decider, err = f(p.Decision, render); err != nil {
			return cfg, fmt.Errorf("decision backend %s: %w", p.DecisionBackend, err)
		}
	}
	if on(p.ReasoningBackend) {
		f, err := reasoners.get("reasoning backend", p.ReasoningBackend)
		if err != nil {
			return cfg, err
		}
		var tools []Tool
		for _, n := range p.Tools {
			if n == "" {
				continue
			}
			tf, err := toolsReg.get("tool", n)
			if err != nil {
				return cfg, err
			}
			tools = append(tools, tf(m))
		}
		if cfg.Reasoner, err = f(p.Reasoning, render, tools); err != nil {
			return cfg, fmt.Errorf("reasoning backend %s: %w", p.ReasoningBackend, err)
		}
	}
	return cfg, nil
}

func on(backend string) bool { return backend != "" && backend != "none" }
