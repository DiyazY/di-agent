package spec

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreTodaysFlags(t *testing.T) {
	d := Default()
	if d.Node.Addr != ":8080" {
		t.Errorf("Node.Addr = %q", d.Node.Addr)
	}
	if len(d.Loops.Relief.Levers) != 2 || d.Loops.Relief.Levers[0].Enter != 0.25 {
		t.Errorf("Relief.Levers = %+v", d.Loops.Relief.Levers)
	}
	if d.Loops.Relief.Enabled {
		t.Error("relief must default to off")
	}
	if d.Policy.Defaults.MaxActionsPerHour != 6 {
		t.Errorf("MaxActionsPerHour = %d", d.Policy.Defaults.MaxActionsPerHour)
	}
	if d.Persist.File != "/var/lib/di-agent/state.json" {
		t.Errorf("Persist.File = %q; the map is kept unless told not to", d.Persist.File)
	}
	if d.Runtime.Kind != "dry-run" {
		t.Errorf("Runtime.Kind = %q", d.Runtime.Kind)
	}
	if len(d.Policy.Rules) != 2 || !d.Policy.Rules[0].Freeze || !d.Policy.Rules[1].Freeze {
		t.Errorf("default rules = %+v", d.Policy.Rules)
	}
	if err := d.Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
}

func TestAnEmptyFileIsTheDefault(t *testing.T) {
	got, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Error("an empty document differs from Default()")
	}
}

func TestAFileOverlaysTheDefaults(t *testing.T) {
	got, err := Parse([]byte("loops: {relief: {interval: 30s}}"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Loops.Relief.Interval != 30*time.Second {
		t.Errorf("Interval = %v", got.Loops.Relief.Interval)
	}
	if got.Loops.Relief.Levers[0].Exit != 0.10 {
		t.Errorf("Exit = %v, want the default 0.10", got.Loops.Relief.Levers[0].Exit)
	}
}

func TestEnvironmentIsExpanded(t *testing.T) {
	t.Setenv("KEY", "s3cret")
	got, err := Parse([]byte("explain:\n  api_key: ${KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Explain.APIKey != "s3cret" {
		t.Errorf("APIKey = %q", got.Explain.APIKey)
	}
}

func TestUnknownKeysAreRefused(t *testing.T) {
	_, err := Parse([]byte("loops: {relief: {levers: [{action: cpu.limit, entre: 0.3}]}}"))
	if err == nil || !strings.Contains(err.Error(), "entre") {
		t.Fatalf("err = %v, want one naming entre", err)
	}
}

func TestInvalidValuesAreRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"exit above enter":    "loops: {relief: {levers: [{action: cpu.limit, exit: 0.3}]}}",
		"of_original over 1":  "policy: {defaults: {floor: {cpu.limit: {of_original: 1.5}}}}",
		"threshold above 1":   "loops: {relief: {levers: [{action: cpu.limit, enter: 1.5}]}}",
		"zero settle":         "loops: {relief: {levers: [{action: cpu.limit, settle: 0s}]}}",
		"unknown level":       "loops: {review: {level: sometimes}}",
		"negative cooldown":   "policy: {defaults: {cooldown: -1m}}",
		"zero relief period":  "loops: {relief: {interval: 0s}}",
		"defaults floor null": "policy: {defaults: {floor: null}}",
		"rule of_original 2":  "policy: {rules: [{match: {name: a}, floor: {cpu.limit: {of_original: 2}}}]}",
		"rule cores negative": "policy: {rules: [{match: {name: a}, floor: {cpu.limit: {min: -1}}}]}",
		"expiry under settle": "loops: {relief: {levers: [{action: cpu.limit, settle: 2m, expiry: 1m}]}}",
		"expiry at settle":    "loops: {relief: {levers: [{action: cpu.limit, settle: 2m, expiry: 2m}]}}",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := Parse([]byte("loops: {relief: {levers: [{action: cpu.limit, exit: 0.3}]}}"))
	if err == nil || !strings.Contains(err.Error(), "exit") {
		t.Errorf("err = %v, want one mentioning exit", err)
	}
}

func TestPrintedSpecLoadsBack(t *testing.T) {
	b, err := Default().YAML()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("printed spec does not load back to the default:\n%s", b)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	s := Default()
	s.Explain.APIKey = "k"
	s.Loops.Review.Decision.APIKey = "k"
	s.Loops.Review.Reasoning.APIKey = "k"
	r := s.Redacted()
	for name, v := range map[string]string{
		"explain":   r.Explain.APIKey,
		"decision":  r.Loops.Review.Decision.APIKey,
		"reasoning": r.Loops.Review.Reasoning.APIKey,
	} {
		if v != "[redacted]" {
			t.Errorf("%s key = %q", name, v)
		}
	}
	if s.Explain.APIKey != "k" {
		t.Error("Redacted changed its receiver")
	}
	if e := Default().Redacted(); e.Explain.APIKey != "" || e.Loops.Review.Decision.APIKey != "" {
		t.Error("an empty key must stay empty")
	}
}

func TestALabelMatchTakesOneOrMany(t *testing.T) {
	got, err := Parse([]byte("policy: {rules: [{match: {labels: {a: x, b: [x, y]}}, freeze: true}]}"))
	if err != nil {
		t.Fatal(err)
	}
	l := got.Policy.Rules[0].Match.Labels
	if !reflect.DeepEqual(l["a"], StringList{"x"}) || !reflect.DeepEqual(l["b"], StringList{"x", "y"}) {
		t.Errorf("labels = %v", l)
	}
}

// TestARuleFloorNamesOneBound: a rule's floor overlays the default floor field by
// field, so the effective floor is what is validated.
func TestARuleFloorNamesOneBound(t *testing.T) {
	for _, doc := range []string{
		"policy: {rules: [{match: {name: \"batch-*\"}, floor: {cpu.limit: {min: 0.5}}}]}",
		"policy: {rules: [{match: {name: \"batch-*\"}, floor: {cpu.limit: {of_original: 0.8}}}]}",
		"policy: {defaults: {floor: {cpu.limit: {min: 0.2}}}}",
	} {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

func TestAnUnsetVariableIsRefused(t *testing.T) {
	_, err := Parse([]byte("explain:\n  api_key: ${DI_AGENT_UNSET_A}\n  url: ${DI_AGENT_UNSET_B}\n"))
	if err == nil || !strings.Contains(err.Error(), "DI_AGENT_UNSET_A") || !strings.Contains(err.Error(), "DI_AGENT_UNSET_B") {
		t.Fatalf("err = %v, want one naming both unset variables", err)
	}
}

func TestAVariableSetEmptyIsEmpty(t *testing.T) {
	t.Setenv("KEY", "")
	got, err := Parse([]byte("explain: {api_key: \"${KEY}\"}"))
	if err != nil || got.Explain.APIKey != "" {
		t.Fatalf("APIKey = %q, err %v; want empty, as set", got.Explain.APIKey, err)
	}
}

// TestANullishVariableIsNotTheDefault: a plain ${VAR} set to "" or "null" stays
// that text, so a string field is empty and a number field is refused rather than
// left at its default.
func TestANullishVariableIsNotTheDefault(t *testing.T) {
	for _, v := range []string{"", "null", "~"} {
		t.Setenv("KEY", v)
		got, err := Parse([]byte("explain:\n  api_key: ${KEY}\n"))
		if err != nil || got.Explain.APIKey != v {
			t.Errorf("KEY=%q: APIKey = %q, err %v; want %q", v, got.Explain.APIKey, err, v)
		}
		if _, err := Parse([]byte("loops:\n  relief:\n    levers:\n      - action: cpu.limit\n        enter: ${KEY}\n")); err == nil {
			t.Errorf("KEY=%q: enter loaded; want it refused, not left at its default", v)
		}
	}
}

func TestAnUnterminatedVariableDoesNotEchoTheValue(t *testing.T) {
	_, err := Parse([]byte("explain:\n  api_key: hunter2${oops\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v; want one naming the line, not the value", err)
	}
}

// TestDollarsAreLiteral: $$ is one literal $, and a $ not opening ${ is left
// alone, so a value such as a password or a regular expression survives.
func TestDollarsAreLiteral(t *testing.T) {
	t.Setenv("KEY", "k")
	got, err := Parse([]byte(`explain: {api_key: "a$$b$c${KEY}$$${KEY}$"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "a$b$ck$k$"; got.Explain.APIKey != want {
		t.Errorf("APIKey = %q, want %q", got.Explain.APIKey, want)
	}
}

func TestAnUnterminatedVariableIsRefused(t *testing.T) {
	for _, doc := range []string{`explain: {api_key: "${KEY"}`, `explain: {api_key: "${}"}`} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", doc)
		}
	}
}

// TestAVariableIsAValueNotYAML: what a variable holds is the value it stands in
// for, never more YAML, and a comment is not expanded.
func TestAVariableIsAValueNotYAML(t *testing.T) {
	t.Setenv("KEY", "x}, url: evil")
	got, err := Parse([]byte("# ${DI_AGENT_UNSET_C} in a comment is not expanded\nexplain:\n  api_key: ${KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Explain.APIKey != "x}, url: evil" || got.Explain.URL != Default().Explain.URL {
		t.Errorf("api_key %q, url %q; want the value verbatim and url untouched", got.Explain.APIKey, got.Explain.URL)
	}
	t.Setenv("SHARE", "0.4")
	got, err = Parse([]byte("loops:\n  relief:\n    levers:\n      - action: cpu.limit\n        enter: ${SHARE}\n"))
	if err != nil || got.Loops.Relief.Levers[0].Enter != 0.4 {
		t.Fatalf("enter = %v, err %v; want a number read from a variable", got.Loops.Relief.Levers, err)
	}
}

func TestPolicyNamesAreKnown(t *testing.T) {
	for name, doc := range map[string]string{
		"allow typo":      "policy: {defaults: {allow: [cpu.limt]}}",
		"deny typo":       "policy: {defaults: {deny: [evcit]}}",
		"rule allow typo": "policy: {rules: [{match: {name: a}, allow: [cpu_limit]}]}",
		"rule deny typo":  "policy: {rules: [{match: {name: a}, deny: [cpu.limits]}]}",
		"unknown kind":    "policy: {rules: [{match: {kind: pods}, freeze: true}]}",
		"node-level kind": "policy: {rules: [{match: {kind: disk}, freeze: true}]}",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := Parse([]byte("policy: {rules: [{match: {name: a}, allow: [cpu.limt]}]}"))
	if err == nil || !strings.Contains(err.Error(), "policy.rules[0].allow") || !strings.Contains(err.Error(), "cpu.limt") {
		t.Errorf("err = %v, want one naming the rule and the value", err)
	}
	for _, doc := range []string{
		"policy: {rules: [{match: {kind: pod}, allow: [cpu.limit], deny: [evict]}]}",
		"policy: {rules: [{match: {kind: unit}, freeze: true}]}",
	} {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

// TestErrorsNameTheLineAsWritten: expansion does not renumber the file, so an
// unknown key or a bad value is reported at the line the operator wrote it on.
func TestErrorsNameTheLineAsWritten(t *testing.T) {
	t.Setenv("SHARE", "high")
	for doc, want := range map[string]string{
		"# ${DI_AGENT_UNSET_D}\n\nloops:\n\n  relief:\n    levers:\n      - action: cpu.limit\n        entre: 0.3\n": "line 8",
		"# comment\n\nloops:\n\n  relief:\n    levers:\n      - action: cpu.limit\n        enter: ${SHARE}\n":        "line 8",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want one at %s", err, want)
		}
	}
}

// TestPersistCanBeTurnedOff: "" keeps the map in memory, over the default path.
func TestPersistCanBeTurnedOff(t *testing.T) {
	got, err := Parse([]byte("persist:\n  file: \"\"\n"))
	if err != nil || got.Persist.File != "" {
		t.Fatalf("Persist.File = %q, err %v; want empty", got.Persist.File, err)
	}
}

// TestTheExampleIsTheDefaults: agent.example.yaml is what -print-config prints, so
// it cannot drift from the defaults it documents.
func TestTheExampleIsTheDefaults(t *testing.T) {
	b, err := os.ReadFile("../../../agent.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	gotYAML, err := got.YAML()
	if err != nil {
		t.Fatal(err)
	}
	want, err := Default().YAML()
	if err != nil {
		t.Fatal(err)
	}
	if string(gotYAML) != string(want) {
		t.Fatalf("agent.example.yaml differs from the defaults; regenerate it with agent -print-config")
	}
}

// TestTheAgentReviewsOnItsOwnByDefault: discovery's candidates are settled by the
// agent — rules, a decision model, and a reasoning model when that is not sure — at
// full autonomy. A person reviewing is a mode an operator turns on, not the default:
// nobody checks thousands of machines one candidate at a time.
func TestTheAgentReviewsOnItsOwnByDefault(t *testing.T) {
	r := Default().Loops.Review
	if r.Level != "full" {
		t.Errorf("review level %q by default; want full", r.Level)
	}
	if r.Decision.Backend == "none" || r.Reasoning.Backend != "openai-tools" {
		t.Errorf("decision %q, reasoning %q by default; want both model tiers on", r.Decision.Backend, r.Reasoning.Backend)
	}
}
