package scripted

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalScenario = `{
  "name": "t", "seed": 1, "tick_seconds": 10, "duration_seconds": 600, "noise": 0.01,
  "node": {"node_cpu": {"coupling": "sum", "base": 0.1, "of": "cpu_utilization"},
           "pressure": {"coupling": "logistic", "theta": 0.8, "k": 0.05, "of": "node_cpu"}},
  "subjects": [{"id": "pod:a", "arrive": 0, "depart": 300, "return": 400,
                "properties": {"cpu_utilization": {"pattern": "sine", "min": 0.1, "max": 0.6, "period": 120}}}],
  "expect": {"admitted_within_ticks": 1}
}`

func TestLoadScenarioAndValidate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(minimalScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := LoadScenario(p)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "t" || sc.TickSeconds != 10 || len(sc.Subjects) != 1 || *sc.Subjects[0].Depart != 300 || *sc.Subjects[0].Return != 400 {
		t.Errorf("loaded %+v", sc)
	}
	if sc.Node["pressure"].Coupling != "logistic" || sc.Node["pressure"].Of != "node_cpu" {
		t.Errorf("node couplings %+v", sc.Node)
	}
}

func TestValidateRejectsBadCouplingSubjectAndPattern(t *testing.T) {
	bad := []struct{ name, edit, want string }{
		{"coupling", `"coupling": "sum"`, "unknown coupling"},
		{"subject", `"id": "pod:a"`, "subject"},
		{"pattern", `"pattern": "sine"`, "unknown pattern"},
	}
	for _, b := range bad {
		var s string
		switch b.name {
		case "coupling":
			s = strings.Replace(minimalScenario, b.edit, `"coupling": "magic"`, 1)
		case "subject":
			s = strings.Replace(minimalScenario, b.edit, `"id": "pod/a"`, 1)
		case "pattern":
			s = strings.Replace(minimalScenario, b.edit, `"pattern": "zigzag"`, 1)
		}
		p := filepath.Join(t.TempDir(), b.name+".json")
		_ = os.WriteFile(p, []byte(s), 0o644)
		_, err := LoadScenario(p)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: err=%v; want it to mention %q", b.name, err, b.want)
		}
	}
}

// scenarioJSON builds a valid two-subject scenario and applies one textual edit, so
// each validation rule is exercised from a file that is otherwise runnable.
func scenarioJSON(edit func(string) string) string {
	base := `{"name":"v","seed":1,"tick_seconds":10,"duration_seconds":600,
	  "node":{"node_cpu":{"coupling":"sum","base":0.1,"of":"cpu_utilization"},
	          "pressure":{"coupling":"logistic","theta":0.5,"k":0.05,"of":"node_cpu"},
	          "idle":{"coupling":"none","base":0.02}},
	  "subjects":[{"id":"pod:a","arrive":0,"depart":300,"return":400,
	               "properties":{"cpu_utilization":{"pattern":"sine","min":0.1,"max":0.6,"period":100}}}],
	  "expect":{"counterfactuals":[
	     {"target":"node_cpu","assume":{"cpu_utilization@pod:a":0.5},"regime":"linear","tolerance":0.05},
	     {"target":"pressure","assume":{"cpu_utilization@pod:a":0.9},"regime":"saturated","min_error":0.05}]}}`
	return edit(base)
}

// TestValidateCoversEveryRule: a scenario is ground truth for the runner, so a field
// the loader does not check is a way to manufacture a wrong truth without an error.
func TestValidateCoversEveryRule(t *testing.T) {
	rep := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("edit target missing: " + old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	cases := []struct {
		name string
		edit func(string) string
		want string
	}{
		{"empty name", rep(`"name":"v"`, `"name":""`), "name"},
		{"zero tick", rep(`"tick_seconds":10`, `"tick_seconds":0`), "tick_seconds"},
		{"node property is not a metric type", rep(`"idle":{`, `"idle@x":{`), "metric type"},
		{"unknown coupling", rep(`"coupling":"none"`, `"coupling":"magic"`), "unknown coupling"},
		{"sum without of", rep(`"coupling":"sum","base":0.1,"of":"cpu_utilization"`, `"coupling":"sum","base":0.1`), "`of`"},
		{"of names nothing", rep(`"of":"cpu_utilization"`, `"of":"cpu_utilisation"`), "names no"},
		{"logistic k <= 0", rep(`"k":0.05`, `"k":0`), "k > 0"},
		{"logistic of logistic", rep(`"idle":{"coupling":"none","base":0.02}`, `"idle":{"coupling":"logistic","theta":0.5,"k":0.05,"of":"pressure"}`), "logistic"},
		{"bad subject id", rep(`"id":"pod:a"`, `"id":"pod/a"`), "subject"},
		{"no properties", rep(`"properties":{"cpu_utilization":{"pattern":"sine","min":0.1,"max":0.6,"period":100}}`, `"properties":{}`), "no properties"},
		{"property is not a metric type", rep(`"cpu_utilization":{"pattern":"sine"`, `"cpu@x":{"pattern":"sine"`), "metric type"},
		{"unknown pattern", rep(`"pattern":"sine"`, `"pattern":"zigzag"`), "unknown pattern"},
		{"empty range", rep(`"period":100}`, `"period":100,"range":[1,1]}`), "empty range"},
		{"departs before arriving", rep(`"arrive":0,"depart":300`, `"arrive":350,"depart":300`), "departs"},
		{"returns before departing", rep(`"return":400`, `"return":200`), "returns"},
		{"bad regime", rep(`"regime":"linear"`, `"regime":"curvy"`), "regime"},
		{"linear without tolerance", rep(`"regime":"linear","tolerance":0.05`, `"regime":"linear"`), "tolerance"},
		{"saturated without min_error", rep(`"regime":"saturated","min_error":0.05`, `"regime":"saturated"`), "min_error"},
		{"counterfactual target is not a node property", rep(`"target":"node_cpu"`, `"target":"node_gpu"`), "target"},
		{"assume key names no subject property", rep(`"cpu_utilization@pod:a":0.5`, `"cpu_utilisation@pod:a":0.5`), "assume"},
		{"assume key without a subject", rep(`"cpu_utilization@pod:a":0.9`, `"cpu_utilization":0.9`), "assume"},
	}
	if _, err := loadFrom(t, scenarioJSON(func(s string) string { return s })); err != nil {
		t.Fatalf("the base scenario must be valid: %v", err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadFrom(t, scenarioJSON(c.edit))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err=%v; want a refusal mentioning %q", err, c.want)
			}
		})
	}
}

func loadFrom(t *testing.T, body string) (*Scenario, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadScenario(p)
}
