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
