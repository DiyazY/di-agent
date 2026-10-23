// Package spec is the agent's one configuration document, agent.yaml. It
// replaces the command line: every setting the daemon takes lives here, with a
// default that is exactly what the flag it replaced defaulted to.
//
// Defaults come first and the file overlays them, so a file names only what it
// changes. Unknown keys are refused rather than ignored, because a misspelt
// threshold that silently keeps its default is a setting that describes nothing.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/autonomy"
)

// Redaction is what a secret is replaced by when a spec is shown.
const Redaction = "[redacted]"

// Spec is everything one agent is told about itself.
type Spec struct {
	Node      Node         `yaml:"node"`
	Profile   Profile      `yaml:"profile"`
	Sources   Sources      `yaml:"sources"`
	Facts     []FactSource `yaml:"facts"`
	Map       MapSpec      `yaml:"map"`
	Persist   Persist      `yaml:"persist"`
	Peers     Peers        `yaml:"peers"`
	Domain    Domain       `yaml:"domain"`
	Explain   Explain      `yaml:"explain"`
	Runtime   RuntimeSpec  `yaml:"runtime"`
	Actuation Actuation    `yaml:"actuation"`
	Loops     Loops        `yaml:"loops"`
	Policy    PolicySpec   `yaml:"policy"`
	Groups    Groups       `yaml:"groups"`
}

// Groups says how the node's subjects are grouped: pods by a label an operator sets,
// units by name. A group whose members use the machine (pods, units) is a group:
// subject of the map, its CPU and memory the sums of its members'.
type Groups struct {
	Interval  time.Duration       `yaml:"interval"`
	LabelKeys []string            `yaml:"label_keys"`
	Units     map[string][]string `yaml:"units"`
}

// Node is the machine the agent models. ID empty falls back to the hostname.
type Node struct {
	Addr        string `yaml:"addr"`
	ID          string `yaml:"id"`
	IngestScope string `yaml:"ingest_scope"`
}

// Profile is the deployment profile and the dynamics of the map's estimates.
type Profile struct {
	Name        string  `yaml:"name"`
	KD          string  `yaml:"kd"`
	Priors      string  `yaml:"priors"`
	Regime      string  `yaml:"regime"`
	Alpha       float64 `yaml:"alpha"`
	AlphaSlow   float64 `yaml:"alpha_slow"`
	Convergence float64 `yaml:"convergence"`
	MinTrust    float64 `yaml:"min_trust"`
}

// Sources is where telemetry comes from. Interval 0 disables the collection loop.
type Sources struct {
	Interval time.Duration `yaml:"interval"`
	// Collectors is the ordered list of telemetry collectors; the order is the
	// ownership priority for node-level types. An empty list disables the loop.
	Collectors []CollectorEntry `yaml:"collectors"`
}

// CollectorEntry is one collector instance. Exactly one of Type (a registered
// built-in), Exec (an out-of-process plugin) or HTTP (a remote plugin) names
// where it comes from. Config is kept as a node because its keys belong to the
// collector, which checks them, not to this schema.
type CollectorEntry struct {
	Name    string        `yaml:"name"`
	Type    string        `yaml:"type,omitempty"`
	Exec    *ExecSpec     `yaml:"exec,omitempty"`
	HTTP    *HTTPSpec     `yaml:"http,omitempty"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
	Config  yaml.Node     `yaml:"config,omitempty"`
}

// ExecSpec is a plugin run as a child process speaking the exec wire protocol.
type ExecSpec struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args,omitempty"`
}

// HTTPSpec is a plugin reached over HTTP.
type HTTPSpec struct {
	URL string `yaml:"url"`
}

// Kind says which source the entry names: "type", "exec" or "http". An entry
// naming none or several returns "", which Validate refuses.
func (e CollectorEntry) Kind() string {
	kinds := e.kinds()
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

func (e CollectorEntry) kinds() []string {
	var k []string
	if e.Type != "" {
		k = append(k, "type")
	}
	if e.Exec != nil {
		k = append(k, "exec")
	}
	if e.HTTP != nil {
		k = append(k, "http")
	}
	return k
}

// FactSource names one source of the machine's facts and where its filesystem
// is mounted, for an agent in a container reading its host.
type FactSource struct {
	Kind string `yaml:"kind"`
	Root string `yaml:"root"`
}

// MapSpec is how the semantic map learns, discovers and forgets.
type MapSpec struct {
	Proposer      Proposer      `yaml:"proposer"`
	PairWindow    time.Duration `yaml:"pair_window"`
	PairSupport   int           `yaml:"pair_support"`
	PairHistory   int           `yaml:"pair_history"`
	StaleAfter    time.Duration `yaml:"stale_after"`
	RetireAfter   time.Duration `yaml:"retire_after"`
	Learn         bool          `yaml:"learn"`
	Admit         bool          `yaml:"admit"`
	SweepInterval time.Duration `yaml:"sweep_interval"`
	JournalSize   int           `yaml:"journal_size"`
	Tuner         bool          `yaml:"tuner"`
}

// Proposer is relationship discovery. Enabled false turns it off for low-CPU devices.
type Proposer struct {
	Enabled   bool    `yaml:"enabled"`
	Threshold float64 `yaml:"threshold"`
	MinPairs  int     `yaml:"min_pairs"`
	MinSpread float64 `yaml:"min_spread"`
	FDR       float64 `yaml:"fdr"`
	MaxPairs  int     `yaml:"max_pairs"`
}

// DefaultStateFile is where the map is snapshotted unless persist.file says
// otherwise.
const DefaultStateFile = "/var/lib/di-agent/state.json"

// Persist is where the map and its journal are snapshotted. An empty File keeps
// everything in memory; the default is a path under /var/lib, so an agent that has
// watched a machine keeps what it learned across restarts unless told not to.
type Persist struct {
	File     string        `yaml:"file"`
	Interval time.Duration `yaml:"interval"`
}

// Peers are the other agents this one knows of.
type Peers struct {
	URLs       []string      `yaml:"urls"`
	StatePoll  time.Duration `yaml:"state_poll"`
	StateStale time.Duration `yaml:"state_stale"`
}

// Domain is the path to domain_spec.json; empty searches upward from the working directory,
// and "none" runs without a domain model whatever the search would find.
type Domain struct {
	Path string `yaml:"path"`
}

// Explain is the natural-language explain backend.
type Explain struct {
	Provider      string `yaml:"provider"`
	URL           string `yaml:"url"`
	Model         string `yaml:"model"`
	Prompt        string `yaml:"prompt"`
	APIKey        string `yaml:"api_key" secret:"true"`
	PlannerPrompt string `yaml:"planner_prompt"`
	CriticPrompt  string `yaml:"critic_prompt"`
	KeepAlive     string `yaml:"keep_alive"`
	Sessions      bool   `yaml:"sessions"`
}

// RuntimeSpec is the orchestrator the agent acts on. Node is the orchestrator's
// name for this machine; empty means Node.ID.
type RuntimeSpec struct {
	Kind       string `yaml:"kind"`
	Kubeconfig string `yaml:"kubeconfig"`
	Node       string `yaml:"node"`
}

// Actuation is where the agent keeps the record of what it changed. Ledger
// empty is actions.json beside persist.file (/var/lib/di-agent/actions.json when
// that is empty too).
type Actuation struct {
	Ledger string `yaml:"ledger"`
}

// Loops are the agent's autonomous activities.
type Loops struct {
	Review Review `yaml:"review"`
	Relief Relief `yaml:"relief"`
}

// Review is the reviewer of proposer candidates.
type Review struct {
	Level      string          `yaml:"level"`
	Interval   time.Duration   `yaml:"interval"`
	PerCycle   int             `yaml:"per_cycle"`
	Rules      []string        `yaml:"rules"`
	Renderer   string          `yaml:"renderer"`
	Thresholds ReviewThreshold `yaml:"thresholds"`
	Decision   Decision        `yaml:"decision"`
	Reasoning  Reasoning       `yaml:"reasoning"`
}

// ReviewThreshold is the probability each verdict needs from a model tier.
type ReviewThreshold struct {
	Confirm float64 `yaml:"confirm"`
	Reject  float64 `yaml:"reject"`
	Defer   float64 `yaml:"defer"`
}

// Decision is review tier 1, the fast decision model.
type Decision struct {
	Backend         string `yaml:"backend"`
	URL             string `yaml:"url"`
	Model           string `yaml:"model"`
	KeepAlive       string `yaml:"keep_alive"`
	ReasoningEffort string `yaml:"reasoning_effort"`
	APIKey          string `yaml:"api_key" secret:"true"`
	// Timeout is how long one call may take. A decision model on the node's own CPUs
	// scoring a candidate with a reasoning model's analysis needs minutes, not seconds.
	Timeout time.Duration `yaml:"timeout"`
}

// Reasoning is review tier 2, the model that may call read-only tools.
type Reasoning struct {
	Backend   string   `yaml:"backend"`
	URL       string   `yaml:"url"`
	Model     string   `yaml:"model"`
	KeepAlive string   `yaml:"keep_alive"`
	Tools     []string `yaml:"tools"`
	APIKey    string   `yaml:"api_key" secret:"true"`
	// Timeout is how long one analysis may take, its tool calls included.
	Timeout time.Duration `yaml:"timeout"`
}

// Relief is the relief loops: one per lever, each relieving one kind of node
// pressure through one action. DryRun nil is the runtime's say, a dry run
// exactly when runtime.kind is dry-run; true on a runtime that changes the
// system decides, records and verifies without changing anything.
type Relief struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	DryRun   *bool         `yaml:"dry_run"`
	Levers   []Lever       `yaml:"levers"`
}

// IsDryRun reports whether relief changes nothing on a runtime of this kind.
func (r Relief) IsDryRun(runtimeKind string) bool {
	if r.DryRun != nil {
		return *r.DryRun
	}
	return runtimeKind == "dry-run"
}

// Lever tunes the relief of one action. Enter and Exit are levels of the node
// pressure the action's actuator relieves, a share in [0,1] (for cpu.limit,
// cpu_contention_ratio: the share of time some workload waited for a CPU
// another was using — PSI stall less the workload's own throttling, not
// utilisation), with Exit below Enter so a hot node is not released the moment
// it cools by a hair. MinEffect is a planning floor: a step whose predicted drop
// in pressure is below it is not taken. It is not the bar a step is judged by
// afterwards: verify keeps a step whose observed drop is at least half the drop
// it predicted, and reverts it otherwise. Expiry must be longer than Settle, or a
// change would be given back before it is judged.
type Lever struct {
	Action      string        `yaml:"action"`
	Enter       float64       `yaml:"enter"`
	Exit        float64       `yaml:"exit"`
	EnterFor    time.Duration `yaml:"enter_for"`
	ExitFor     time.Duration `yaml:"exit_for"`
	Step        float64       `yaml:"step"`
	MinEffect   float64       `yaml:"min_effect"`
	VictimShare float64       `yaml:"victim_share"`
	Settle      time.Duration `yaml:"settle"`
	Expiry      time.Duration `yaml:"expiry"`
}

// defaultLevers is the levers relief has unless the file names its own.
func defaultLevers() []Lever {
	return []Lever{{
		Action: "cpu.limit", Enter: 0.25, Exit: 0.10, EnterFor: time.Minute, ExitFor: 3 * time.Minute,
		Step: 0.25, MinEffect: 0.03, VictimShare: 0.05, Settle: 2 * time.Minute, Expiry: time.Hour,
	}}
}

// UnmarshalYAML starts a lever from the default lever for its action, so a file
// names only what it changes, and refuses a key a lever does not have: a lever
// is decoded on its own, outside the strict pass that catches every other
// misspelt key.
func (l *Lever) UnmarshalYAML(n *yaml.Node) error {
	type plain Lever
	errs := unknownFields(n, reflect.TypeOf(plain{}), "spec.Lever")
	var p plain
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != "action" {
				continue
			}
			for _, d := range defaultLevers() {
				if d.Action == n.Content[i+1].Value {
					p = plain(d)
				}
			}
		}
	}
	if err := n.Decode(&p); err != nil {
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			return err
		}
		errs = append(errs, te.Errors...)
	}
	*l = Lever(p)
	if len(errs) > 0 {
		return &yaml.TypeError{Errors: errs}
	}
	return nil
}

// unknownFields is an error, in the decoder's own words, for each key of the
// mapping n that is not a yaml field of t.
func unknownFields(n *yaml.Node, t reflect.Type, name string) []string {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	known := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		k, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		known[k] = true
	}
	var out []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; !known[k.Value] {
			out = append(out, fmt.Sprintf("line %d: field %s not found in type %s", k.Line, k.Value, name))
		}
	}
	return out
}

// Bound is the least one action may cut a setting to: an absolute value in the
// actuator's unit, or a share of the workload's original setting, whichever is
// higher. For an action whose original may be unset, as memory.high's, the
// original is the usage when the agent first acted.
type Bound struct {
	Min        *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	OfOriginal *float64 `yaml:"of_original,omitempty" json:"of_original,omitempty"`
}

// NewBound is a bound with both values set.
func NewBound(min, ofOriginal float64) Bound {
	return Bound{Min: &min, OfOriginal: &ofOriginal}
}

// over is b with every value it leaves unset taken from base, sharing nothing
// with either.
func (b Bound) over(base Bound) Bound {
	var out Bound
	for _, src := range []Bound{base, b} {
		if src.Min != nil {
			v := *src.Min
			out.Min = &v
		}
		if src.OfOriginal != nil {
			v := *src.OfOriginal
			out.OfOriginal = &v
		}
	}
	return out
}

// Floor is the bound of each action, by action name. A rule names the actions
// and bounds it changes; every one it leaves out is the default's.
type Floor map[string]Bound

// Values is the action's two bounds, zero for one not set or an action the floor
// does not name.
func (f Floor) Values(action string) (min, ofOriginal float64) {
	b := f[action]
	if b.Min != nil {
		min = *b.Min
	}
	if b.OfOriginal != nil {
		ofOriginal = *b.OfOriginal
	}
	return min, ofOriginal
}

// Over is f with every action and bound it leaves unset taken from base, as a
// new floor that shares nothing with either. Both nil is nil.
func (f Floor) Over(base Floor) Floor {
	if f == nil && base == nil {
		return nil
	}
	out := make(Floor, len(base)+len(f))
	for a, b := range base {
		out[a] = b.over(Bound{})
	}
	for a, b := range f {
		out[a] = b.over(out[a])
	}
	return out
}

// Settings is what a policy permits. A rule's zero values inherit the defaults.
type Settings struct {
	Allow             []string      `yaml:"allow" json:"allow"`
	Deny              []string      `yaml:"deny" json:"deny"`
	Floor             Floor         `yaml:"floor,omitempty" json:"floor"`
	MaxActionsPerHour int           `yaml:"max_actions_per_hour" json:"max_actions_per_hour"`
	Cooldown          time.Duration `yaml:"cooldown" json:"cooldown"`
}

// settingsJSON is Settings on the wire: the cooldown as a Go duration string,
// as agent.yaml writes it and /config shows it, rather than nanoseconds.
type settingsJSON struct {
	Allow             []string `json:"allow"`
	Deny              []string `json:"deny"`
	Floor             Floor    `json:"floor"`
	MaxActionsPerHour int      `json:"max_actions_per_hour"`
	Cooldown          string   `json:"cooldown"`
}

// MarshalJSON renders the cooldown as "10m0s".
func (s Settings) MarshalJSON() ([]byte, error) {
	return json.Marshal(settingsJSON{Allow: s.Allow, Deny: s.Deny, Floor: s.Floor,
		MaxActionsPerHour: s.MaxActionsPerHour, Cooldown: s.Cooldown.String()})
}

// UnmarshalJSON reads what MarshalJSON writes.
func (s *Settings) UnmarshalJSON(b []byte) error {
	var w settingsJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	var d time.Duration
	if w.Cooldown != "" {
		var err error
		if d, err = time.ParseDuration(w.Cooldown); err != nil {
			return fmt.Errorf("cooldown: %w", err)
		}
	}
	*s = Settings{Allow: w.Allow, Deny: w.Deny, Floor: w.Floor, MaxActionsPerHour: w.MaxActionsPerHour, Cooldown: d}
	return nil
}

// Match selects workloads. Every field set must hold; a label matches when the
// workload carries any one of its values.
type Match struct {
	Kind   string                `yaml:"kind"`
	Name   string                `yaml:"name"`
	Labels map[string]StringList `yaml:"labels"`
}

// Rule applies Settings to the workloads it matches, or freezes them: a frozen
// workload is never acted on. Settings' MarshalJSON is promoted, so a Rule is
// not served as JSON; /config renders the spec through its YAML.
type Rule struct {
	Match    Match `yaml:"match"`
	Freeze   bool  `yaml:"freeze"`
	Settings `yaml:",inline"`
}

// PolicySpec is what the agent may do to a workload.
type PolicySpec struct {
	Defaults Settings `yaml:"defaults"`
	Rules    []Rule   `yaml:"rules"`
}

// StringList reads either one string or a list of them, so a label can be
// written as `a: x` or `a: [x, y]`.
type StringList []string

// UnmarshalYAML accepts a scalar or a sequence.
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*l = StringList{s}
		return nil
	}
	var ss []string
	if err := n.Decode(&ss); err != nil {
		return err
	}
	*l = ss
	return nil
}

// Default is today's behaviour: each value is the default of the flag it replaced.
// Slices are empty rather than nil so that a printed spec loads back equal.
func Default() Spec {
	return Spec{
		Node: Node{Addr: ":8080", IngestScope: "self"},
		Profile: Profile{
			Name: "edge-minimal", Alpha: 0.2, AlphaSlow: 0.001, Convergence: 500, MinTrust: 0.5,
		},
		Sources: Sources{
			Interval: 10 * time.Second,
			// The file's list replaces this one: decoding into a non-empty slice
			// restarts it, so naming collectors never adds to the default.
			Collectors: []CollectorEntry{{Name: "cgroup", Type: "cgroup"}},
		},
		Facts: []FactSource{{Kind: "host"}},
		Map: MapSpec{
			Proposer: Proposer{
				Enabled: true, Threshold: 0.85, MinPairs: 30,
				MinSpread: minimal.DefaultMinSpread, FDR: minimal.DefaultFDR,
			},
			PairWindow: 15 * time.Second,
			StaleAfter: 2 * time.Minute, RetireAfter: 10 * time.Minute,
			Learn: true, Admit: true, Tuner: true,
		},
		Persist: Persist{File: DefaultStateFile, Interval: time.Minute},
		Peers:   Peers{URLs: []string{}, StatePoll: 30 * time.Second, StateStale: 90 * time.Second},
		Explain: Explain{
			Provider: "none", URL: "http://localhost:11434/v1", Model: "qwen2.5:7b-instruct",
			KeepAlive: "30m", Sessions: true,
		},
		Runtime: RuntimeSpec{Kind: "dry-run"},
		Loops: Loops{
			Review: Review{
				Level: "full", Interval: time.Minute, PerCycle: 10,
				Rules: []string{"composition"}, Renderer: "text",
				Thresholds: ReviewThreshold{
					Confirm: autonomy.DefaultThresholds.Confirm,
					Reject:  autonomy.DefaultThresholds.Reject,
					Defer:   autonomy.DefaultThresholds.Defer,
				},
				Decision: Decision{
					Backend: "systemone", URL: "http://localhost:11434",
					Model: "tev1:4b", ReasoningEffort: "none", Timeout: 5 * time.Minute,
				},
				Reasoning: Reasoning{
					Backend: "openai-tools", URL: "http://localhost:11434/v1",
					Model: "qwen3.8:27b-nvfp4", KeepAlive: "10m", Timeout: 10 * time.Minute,
					Tools: slices.Clone(autonomy.DefaultTools),
				},
			},
			Relief: Relief{Interval: 15 * time.Second, Levers: defaultLevers()},
		},
		Groups: Groups{Interval: 30 * time.Second,
			LabelKeys: []string{"di-agent/group", "app.kubernetes.io/part-of"},
			Units:     map[string][]string{"system": {"*"}}},
		Policy: PolicySpec{
			Defaults: Settings{
				Allow: []string{"cpu.limit", "memory.high"}, Deny: []string{"evict"},
				Floor: Floor{
					"cpu.limit":   NewBound(0.1, 0.5),
					"memory.high": NewBound(64<<20, 0),
				},
				MaxActionsPerHour: 6, Cooldown: 10 * time.Minute,
			},
			Rules: []Rule{
				{
					Match:    Match{Labels: map[string]StringList{"di-agent/freeze": {"true"}}},
					Freeze:   true,
					Settings: Settings{Allow: []string{}, Deny: []string{}},
				},
				{
					Match: Match{Labels: map[string]StringList{
						"k8s/priority-class": {"system-node-critical", "system-cluster-critical"},
					}},
					Freeze:   true,
					Settings: Settings{Allow: []string{}, Deny: []string{}},
				},
			},
		},
	}
}

// Parse overlays one document on Default(). ${VAR} in a value is replaced from
// the environment, so a secret can stay out of the file. A variable that is not
// set is an error naming it, rather than an empty value that describes nothing;
// one set to the empty string is empty. $$ is a literal $, and any other $ is
// left as it is. Variables are expanded in values only, after the document is
// read, so what a variable holds is the value it stands in for and never more
// YAML, and a comment mentioning one is not expanded.
func Parse(b []byte) (Spec, error) {
	s := Default()
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	if doc.Kind == 0 {
		return s, nil // an empty document is the default
	}
	if err := unknownKeys(b); err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	if err := expandNode(&doc); err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	// The expanded node keeps the file's line numbers, so an error names the line
	// the operator wrote.
	if err := doc.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	// A floor's bound is decoded whole, so the defaults' floor is overlaid on the
	// built-in one per action and per bound, as a rule's is on the defaults'. A
	// floor written null is left so, for Validate to refuse.
	if f := s.Policy.Defaults.Floor; f != nil {
		s.Policy.Defaults.Floor = f.Over(Default().Policy.Defaults.Floor)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// unknownKeys refuses every key the spec does not have, at the line it is on in
// b. A node does not decode strictly, so the file as written is decoded strictly
// on its own, and only its unknown-key errors are kept: a type error there is a
// ${VAR} not yet expanded, and the expanded document's own decode reports any
// that remain.
func unknownKeys(b []byte) error {
	probe := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	err := dec.Decode(&probe)
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return nil // a syntax error was reported when the document was read
	}
	var unknown []string
	for _, e := range te.Errors {
		if strings.Contains(e, " not found in type ") {
			unknown = append(unknown, e)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return &yaml.TypeError{Errors: unknown}
}

// expandNode expands ${VAR} in every scalar under n, collecting every variable
// that is not set and every malformed reference into one error. A plain scalar
// that changed has its tag cleared, so its new text is typed afresh: enter:
// ${SHARE} with SHARE=0.4 is a number, as if 0.4 had been written; a quoted one
// stays a string.
func expandNode(n *yaml.Node) error {
	var errs []error
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			v, err := expand(n.Value)
			if err != nil {
				errs = append(errs, fmt.Errorf("line %d: %w", n.Line, err))
				return
			}
			if v != n.Value {
				n.Value = v
				if n.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
					n.Tag = ""
					// A value that reads as null would leave the field at its
					// default without a word. Kept a string, an empty api_key is
					// empty and an empty enter is refused as not a number.
					switch v {
					case "", "~", "null", "Null", "NULL":
						n.Tag = "!!str"
					}
				}
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return errors.Join(errs...)
}

// expand replaces each ${VAR} in s with the variable's value and each $$ with $.
// Any other $ is kept. Every unset variable is named in the error.
func expand(s string) (string, error) {
	var (
		out   strings.Builder
		unset []string
	)
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			out.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			out.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", errors.New("unterminated ${ (write $$ for a literal $)")
			}
			name := s[i+2 : i+2+end]
			if name == "" {
				return "", fmt.Errorf("empty ${} in %q", s)
			}
			v, ok := os.LookupEnv(name)
			if !ok {
				unset = append(unset, name)
			}
			out.WriteString(v)
			i += 2 + end
		default:
			out.WriteByte('$')
		}
	}
	if len(unset) > 0 {
		return "", fmt.Errorf("environment variable not set: %s", strings.Join(unset, ", "))
	}
	return out.String(), nil
}

// Load reads a spec file; an empty path is the default spec.
func Load(path string) (Spec, error) {
	if path == "" {
		return Default(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	s, err := Parse(b)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// PolicyActions are the names a policy's allow and deny lists and its floors may
// hold. They are the actuators' actions (cpu.limit and memory.high; a test in
// pkg/runtime/kubernetes holds every registered one to this list), named here
// because those packages import this one. "evict" is reserved: no actuator performs it, and the default
// policy denies it so that one that one day can will not do so by default.
var PolicyActions = []string{"cpu.limit", "evict", "memory.high"}

// WorkloadKinds are the subject kinds, the part of a workload's subject before
// the colon, that a policy rule's match.kind may name: the kinds of the
// workloads the agent's sources and runtimes name (pod and container from
// Kubernetes, unit from systemd, app from Netdata's apps groups). Node-level
// instances such as disk, net or cpu are not workloads and match nothing.
var WorkloadKinds = []string{"app", "container", "pod", "unit"}

// Validate reports every value that cannot be what the operator meant.
func (s Spec) Validate() error {
	var errs []error
	share := func(name string, v float64) {
		if v < 0 || v > 1 {
			errs = append(errs, fmt.Errorf("%s = %v, want a value in [0,1]", name, v))
		}
	}
	positive := func(name string, d time.Duration) {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s = %v, want a positive duration", name, d))
		}
	}
	notNegative := func(name string, d time.Duration) {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s = %v, want a duration of zero or more", name, d))
		}
	}
	// floor checks an effective floor: every action it names is an action, with
	// both bounds set and each in range.
	floor := func(name string, f Floor) {
		actions := make([]string, 0, len(f))
		for a := range f {
			actions = append(actions, a)
		}
		slices.Sort(actions)
		for _, a := range actions {
			b, at := f[a], name+"."+a
			if !slices.Contains(PolicyActions, a) {
				errs = append(errs, fmt.Errorf("%s: %q is not an action, want one of %s", name, a, strings.Join(PolicyActions, ", ")))
				continue
			}
			if b.Min == nil || b.OfOriginal == nil {
				errs = append(errs, fmt.Errorf("%s must set both min and of_original", at))
				continue
			}
			if *b.Min < 0 {
				errs = append(errs, fmt.Errorf("%s.min = %v, want zero or more", at, *b.Min))
			}
			if *b.OfOriginal < 0 || *b.OfOriginal > 1 {
				errs = append(errs, fmt.Errorf("%s.of_original = %v, want a value in [0,1]", at, *b.OfOriginal))
			}
		}
	}

	notNegative("sources.interval", s.Sources.Interval)
	// now_unix and timestamp_unix are whole seconds, so a faster tick cannot
	// give each collect a timestamp of its own.
	if s.Sources.Interval > 0 && s.Sources.Interval < time.Second {
		errs = append(errs, fmt.Errorf("sources.interval = %v, want 0 (off) or at least 1s: timestamps are whole seconds", s.Sources.Interval))
	}
	errs = append(errs, s.Sources.validateCollectors()...)
	notNegative("map.stale_after", s.Map.StaleAfter)
	notNegative("map.retire_after", s.Map.RetireAfter)
	notNegative("persist.interval", s.Persist.Interval)
	notNegative("peers.state_poll", s.Peers.StatePoll)

	if !slices.Contains([]string{"off", "suggest", "rules", "bounded", "full"}, s.Loops.Review.Level) {
		errs = append(errs, fmt.Errorf("loops.review.level = %q, want off, suggest, rules, bounded or full", s.Loops.Review.Level))
	}
	share("loops.review.thresholds.confirm", s.Loops.Review.Thresholds.Confirm)
	share("loops.review.thresholds.reject", s.Loops.Review.Thresholds.Reject)
	share("loops.review.thresholds.defer", s.Loops.Review.Thresholds.Defer)

	r := s.Loops.Relief
	positive("loops.relief.interval", r.Interval)
	if r.DryRun != nil && !*r.DryRun && s.Runtime.Kind == "dry-run" {
		errs = append(errs, errors.New("loops.relief.dry_run = false, but runtime.kind dry-run changes nothing: name a runtime that does, or leave dry_run unset"))
	}
	if r.Enabled && len(r.Levers) == 0 {
		errs = append(errs, errors.New("loops.relief.levers is empty: relief enabled has nothing to act through"))
	}
	seenLever := map[string]bool{}
	for i, c := range r.Levers {
		name := fmt.Sprintf("loops.relief.levers[%d]", i)
		switch {
		case c.Action == "":
			errs = append(errs, fmt.Errorf("%s.action is empty", name))
		case seenLever[c.Action]:
			errs = append(errs, fmt.Errorf("%s %s: duplicate action: one lever per action", name, c.Action))
		}
		seenLever[c.Action] = true
		share(name+".enter", c.Enter)
		share(name+".exit", c.Exit)
		share(name+".step", c.Step)
		share(name+".min_effect", c.MinEffect)
		share(name+".victim_share", c.VictimShare)
		if c.Exit >= c.Enter {
			errs = append(errs, fmt.Errorf("%s.exit = %v must be below enter = %v", name, c.Exit, c.Enter))
		}
		positive(name+".enter_for", c.EnterFor)
		positive(name+".exit_for", c.ExitFor)
		positive(name+".settle", c.Settle)
		positive(name+".expiry", c.Expiry)
		if c.Expiry <= c.Settle {
			// A change that expires before it settles is given back before it is judged.
			errs = append(errs, fmt.Errorf("%s.expiry = %v must be longer than settle = %v", name, c.Expiry, c.Settle))
		}
		if _, ok := s.Policy.Defaults.Floor[c.Action]; c.Action != "" && s.Policy.Defaults.Floor != nil && !ok {
			// Without a floor of its own an action could cut a workload to its actuator's minimum.
			errs = append(errs, fmt.Errorf("policy.defaults.floor has no %s, the action of %s", c.Action, name))
		}
	}

	// actions checks that every name in an allow or deny list is an action the
	// agent knows: a misspelt deny entry would deny nothing.
	actions := func(name string, list []string) {
		for _, a := range list {
			if !slices.Contains(PolicyActions, a) {
				errs = append(errs, fmt.Errorf("%s: %q is not an action, want one of %s", name, a, strings.Join(PolicyActions, ", ")))
			}
		}
	}
	actions("policy.defaults.allow", s.Policy.Defaults.Allow)
	actions("policy.defaults.deny", s.Policy.Defaults.Deny)
	positive("policy.defaults.cooldown", s.Policy.Defaults.Cooldown)
	if s.Policy.Defaults.MaxActionsPerHour < 0 {
		errs = append(errs, fmt.Errorf("policy.defaults.max_actions_per_hour = %d, want zero or more", s.Policy.Defaults.MaxActionsPerHour))
	}
	if s.Policy.Defaults.Floor == nil {
		errs = append(errs, errors.New("policy.defaults.floor is empty: every action needs the least it may cut to"))
	}
	floor("policy.defaults.floor", s.Policy.Defaults.Floor)
	for i, rule := range s.Policy.Rules {
		name := fmt.Sprintf("policy.rules[%d]", i)
		notNegative(name+".cooldown", rule.Cooldown)
		actions(name+".allow", rule.Allow)
		actions(name+".deny", rule.Deny)
		if k := rule.Match.Kind; k != "" && !slices.Contains(WorkloadKinds, k) {
			// A misspelt kind matches nothing, so the rule would silently never apply.
			errs = append(errs, fmt.Errorf("%s.match.kind = %q, want one of %s", name, k, strings.Join(WorkloadKinds, ", ")))
		}
		if rule.Floor != nil {
			// What is checked is the floor the rule takes effect with: its own bounds
			// over the defaults'.
			floor(name+".floor", rule.Floor.Over(s.Policy.Defaults.Floor))
		}
	}
	return errors.Join(errs...)
}

// Redacted returns a copy with every field tagged secret:"true" replaced by
// Redaction when it holds something, so a spec can be logged or served.
//
// A collector entry is redacted too, because the agent cannot know which of its
// values a plugin treats as a credential: its config block becomes Redaction, an
// exec entry's arguments each become Redaction, and an http entry's URL is
// RedactURL's view of it. The command, the URL's host and path, and the
// built-in type stay, so the served spec still says what runs.
func (s Spec) Redacted() Spec {
	redact(reflect.ValueOf(&s).Elem())
	s.Sources.Collectors = redactCollectors(s.Sources.Collectors)
	return s
}

// emptyNode reports whether a config block holds nothing worth withholding: absent,
// null, or an empty mapping or sequence.
func emptyNode(n *yaml.Node) bool {
	switch n.Kind {
	case 0:
		return true
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) == 0
	case yaml.ScalarNode:
		return n.Tag == "!!null"
	}
	return false
}

// redactCollectors returns a redacted copy of entries; the receiver's slice, its
// exec and http specs and its config nodes are left untouched.
func redactCollectors(entries []CollectorEntry) []CollectorEntry {
	if entries == nil {
		return nil
	}
	out := make([]CollectorEntry, len(entries))
	for i, e := range entries {
		if !emptyNode(&e.Config) {
			e.Config = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: Redaction}
		}
		if e.Exec != nil {
			ex := *e.Exec
			if ex.Args != nil {
				ex.Args = make([]string, len(e.Exec.Args))
				for j := range ex.Args {
					ex.Args[j] = Redaction
				}
			}
			e.Exec = &ex
		}
		if e.HTTP != nil {
			e.HTTP = &HTTPSpec{URL: RedactURL(e.HTTP.URL)}
		}
		out[i] = e
	}
	return out
}

// RedactURL is the one view of a configured URL that may be logged, served or
// put in an error: the scheme, host and path stay; userinfo (with or without a
// password, since a bare token is commonly put in the username) becomes
// Redaction; a query becomes "?" + Redaction; a fragment is dropped. A URL that
// does not parse, or an opaque one, is Redaction entirely.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return Redaction
	}
	hadUser := u.User != nil
	hadQuery := u.RawQuery != "" || u.ForceQuery
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	out := u.String()
	if hadUser {
		prefix := u.Scheme + "://"
		if u.Scheme == "" {
			prefix = "//"
		}
		out = prefix + Redaction + "@" + strings.TrimPrefix(out, prefix)
	}
	if hadQuery {
		out += "?" + Redaction
	}
	return out
}

func redact(v reflect.Value) {
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if t.Field(i).Tag.Get("secret") == "true" && f.Kind() == reflect.String {
			if f.String() != "" {
				f.SetString(Redaction)
			}
			continue
		}
		redact(f)
	}
}

// YAML renders the spec as a document Parse reads back to the same spec.
func (s Spec) YAML() ([]byte, error) {
	return yaml.Marshal(s)
}

var collectorName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateCollectors reports every collector entry that cannot be what the
// operator meant. Each message names the entry by index and name.
func (s Sources) validateCollectors() []error {
	var errs []error
	seen := map[string]bool{}
	hasScript := false
	for i, c := range s.Collectors {
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("sources.collectors[%d] %s: %s", i, c.Name, fmt.Sprintf(format, a...)))
		}
		switch {
		case !collectorName.MatchString(c.Name):
			bad("name must match [A-Za-z0-9._-]+")
		case seen[c.Name]:
			bad("duplicate name")
		}
		seen[c.Name] = true
		if k := c.kinds(); len(k) != 1 {
			bad("want exactly one of type, exec or http, got %d", len(k))
		}
		if c.Exec != nil && c.Exec.Command == "" {
			bad("exec.command is empty")
		}
		if c.HTTP != nil && c.HTTP.URL == "" {
			bad("http.url is empty")
		}
		if c.Timeout < 0 {
			bad("timeout = %v, want zero or more", c.Timeout)
		}
		// Strictly less: the supervisor waits timeout plus a short grace for an exec
		// or http member, so a timeout at the interval would let one hung plugin
		// stretch every tick past it.
		if s.Interval > 0 && c.Timeout >= s.Interval {
			bad("timeout = %v must be less than sources.interval = %v", c.Timeout, s.Interval)
		}
		hasScript = hasScript || c.Type == "script"
	}
	if hasScript && len(s.Collectors) > 1 {
		for i, c := range s.Collectors {
			if c.Type == "script" {
				errs = append(errs, fmt.Errorf("sources.collectors[%d] %s: script is exclusive and cannot share the list with another collector", i, c.Name))
			}
		}
	}
	return errs
}
