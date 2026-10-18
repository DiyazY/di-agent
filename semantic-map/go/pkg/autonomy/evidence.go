package autonomy

import (
	"fmt"
	"math"
	"strings"

	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// Renderer turns a candidate's evidence into the text a model is given. The text is
// recorded in the decision verbatim, so a renderer should be deterministic: the same
// evidence, the same text. Its name is recorded with every verdict.
type Renderer interface {
	Name() string
	Render(e Evidence, analysis string) string
}

func init() {
	RegisterRenderer("text", func() Renderer { return textRenderer{} })
}

// textRenderer is plain English for a model that has never seen this system: what
// each metric is, the correlation and its false-discovery check, the direction, the
// kind, the caveats, and the candidates sharing an end.
type textRenderer struct{}

func (textRenderer) Name() string { return "text-v2" }

func (textRenderer) Render(e Evidence, analysis string) string { return stateText(e, analysis) }

func stateText(e Evidence, analysis string) string {
	c := e.Candidate
	var b strings.Builder
	b.WriteString("A candidate relationship, found by correlating one machine's own telemetry. ")
	b.WriteString("Nothing has been declared about it; it is only that two metrics moved together.\n\n")
	if m := machineLine(e.Facts); m != "" {
		fmt.Fprintf(&b, "Machine: %s\n", m)
	}
	fmt.Fprintf(&b, "Source: %s\n", describe(e.From))
	fmt.Fprintf(&b, "Target: %s\n\n", describe(e.To))

	sign := "they rise and fall together"
	if c.Direction == types.Negative {
		sign = "one rises as the other falls"
	}
	fmt.Fprintf(&b, "Correlation: Pearson |r| = %.3f over %d paired readings (%s).\n", c.MIScore, c.NObservations, sign)
	if c.PBar > 0 {
		fmt.Fprintf(&b, "False-discovery check: p = %.2g against a Benjamini–Hochberg bar of %.2g, counting every pair tested; it passed.\n", c.PValue, c.PBar)
	} else {
		fmt.Fprintf(&b, "p = %.2g; no correction for the number of pairs tested was applied.\n", c.PValue)
	}
	if c.Undirected {
		b.WriteString("Direction: unknown. Both are the same kind of property (both node-level, or both a workload's), " +
			"and correlation does not say which drives the other.\n")
	} else {
		b.WriteString("Direction: the source is one workload's or device's property and the target the whole machine's, " +
			"so if either drives the other it is the source driving the target.\n")
	}
	switch c.Kind {
	case types.CandidateKindSameMetric:
		b.WriteString("Pair type: the same metric on two different workloads. When two workloads' readings of one metric " +
			"move together, something driving both (the same client, the same schedule) is the likelier explanation.\n")
	case types.CandidateKindSameSubject:
		b.WriteString("Pair type: two metrics of the same workload. One workload's own quantities often move together by " +
			"construction (a capped workload's CPU and its throttling), which is accounting rather than a finding.\n")
	case types.CandidateKindComposition:
		b.WriteString("Pair type: composition. The source is a part of the target by definition.\n")
	}
	for _, cv := range c.Caveats {
		fmt.Fprintf(&b, "Caveat: %s\n", cv)
	}
	if len(e.Neighbours) > 0 {
		b.WriteString("\nOther candidates sharing an end with this one (a metric that correlates with many others is " +
			"more likely following overall load than driving any one of them):\n")
		for _, n := range e.Neighbours {
			fmt.Fprintf(&b, "- %s: |r| = %.3f over %d readings%s\n", n.CandidateID, n.MIScore, n.NObservations, kindNote(n.Kind))
		}
	}
	if analysis != "" {
		b.WriteString("\nAnalysis by a reasoning agent that examined the machine's state model:\n")
		b.WriteString(strings.TrimSpace(analysis))
		b.WriteString("\n")
	}
	return b.String()
}

// describe is one property in a line: whose it is, what it measures, where it is now.
func describe(p statemap.Property) string {
	var b strings.Builder
	b.WriteString(p.ID)
	switch {
	case p.Subject != "":
		fmt.Fprintf(&b, " — a property of %s", p.Subject)
		if name := p.Labels["name"]; name != "" {
			fmt.Fprintf(&b, " (%s)", name)
		}
	default:
		b.WriteString(" — a property of the whole machine")
	}
	if p.Unit != "" {
		fmt.Fprintf(&b, "; unit %s", p.Unit)
	}
	if p.RangeDeclared && !(p.Range[0] == 0 && p.Range[1] == 0) {
		fmt.Fprintf(&b, "; range [%s, %s]", num(p.Range[0]), num(p.Range[1]))
	}
	fmt.Fprintf(&b, "; now %s over %d observations", num(p.Value), p.NObservations)
	if p.Source != "" {
		fmt.Fprintf(&b, "; read by %s", p.Source)
	}
	return b.String()
}

// machineLine is the facts that frame a value, in one line: what the machine is
// and how much of it there is. Empty when nothing was read.
func machineLine(facts []statemap.Fact) string {
	by := map[string]statemap.Fact{}
	for _, f := range facts {
		by[f.Key] = f
	}
	var parts []string
	if f, ok := by["machine_model"]; ok {
		parts = append(parts, f.Value)
	}
	if f, ok := by["cpu_cores"]; ok {
		cpu := f.Value + " CPU cores"
		if m, ok := by["cpu_model"]; ok {
			cpu += " (" + m.Value + ")"
		}
		parts = append(parts, cpu)
	}
	for _, k := range []string{"memory_total", "disk_total"} {
		if f, ok := by[k]; ok && f.Number != nil {
			parts = append(parts, fmt.Sprintf("%.1f GiB %s", *f.Number/(1<<30), strings.TrimSuffix(k, "_total")))
		}
	}
	if f, ok := by["os"]; ok {
		parts = append(parts, f.Value)
	}
	return strings.Join(parts, ", ")
}

func kindNote(kind string) string {
	if kind == "" {
		return ""
	}
	return " (" + kind + ")"
}

func num(v float64) string {
	if v != 0 && (math.Abs(v) >= 1e6 || math.Abs(v) < 1e-3) {
		return fmt.Sprintf("%.3g", v)
	}
	return fmt.Sprintf("%.4g", v)
}
