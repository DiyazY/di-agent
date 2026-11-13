package explain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// Narrate renders a decision and the chain it rests on as prose: the decision's
// line, then one "rests on" line per link in the order given. No LLM, no live
// state, no clock: a pure function of the records, so a decision read back from
// a snapshot or the journal log narrates exactly as it did when it was made.
func Narrate(d statemap.Decision, cites []statemap.ChainLink) string {
	held := map[string]record{}
	for _, l := range cites {
		if !l.Evicted && l.Decision != nil {
			held[l.ID] = normalise(*l.Decision)
		}
	}
	r := normalise(d)
	lines := []string{heading(r) + ": " + sentence(r, held)}
	for _, l := range cites {
		if c, ok := held[l.ID]; ok {
			lines = append(lines, "  rests on "+heading(c)+": "+sentence(c, held))
		} else {
			lines = append(lines, "  rests on "+l.ID+": no longer held")
		}
	}
	return strings.Join(lines, "\n")
}

// record is the decision after one JSON round-trip (json.Marshal then
// json.Unmarshal into a map[string]any), so every value is a map, []any,
// float64, string, bool or nil whether the producer wrote a relief.Plan, an
// admission.Line, a time.Time, or the snapshot decoded it.
type record struct {
	d statemap.Decision
	m map[string]any
}

// normalise is d as a record. A decision that cannot be encoded, a NaN in its
// answer, keeps its id, kind and time and reads every other value as absent.
func normalise(d statemap.Decision) record {
	r := record{d: d}
	if raw, err := json.Marshal(d); err == nil {
		_ = json.Unmarshal(raw, &r.m)
	}
	return r
}

// get walks maps by key and []any by decimal index.
func (r record) get(path ...string) (any, bool) {
	var v any = r.m
	for _, p := range path {
		switch x := v.(type) {
		case map[string]any:
			e, ok := x[p]
			if !ok {
				return nil, false
			}
			v = e
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// val is the value at path, nil when absent.
func (r record) val(path ...string) any {
	x, _ := r.get(path...)
	return x
}

// str is the string at path, "" when absent or not a string.
func (r record) str(path ...string) string {
	s, _ := r.val(path...).(string)
	return s
}

func (r record) num(path ...string) (float64, bool) {
	f, ok := r.val(path...).(float64)
	return f, ok
}

// v is the value at path as the narrative prints it, "?" when absent.
func (r record) v(path ...string) string { return show(r.val(path...)) }

func (r record) list(path ...string) []any {
	l, _ := r.val(path...).([]any)
	return l
}

// fmt3 is three significant digits.
func fmt3(v float64) string { return strconv.FormatFloat(v, 'g', 3, 64) }

// show prints a normalised value: numbers to three digits, maps as sorted
// k=v, lists comma-joined, nothing as "?".
func show(v any) string {
	switch x := v.(type) {
	case nil:
		return "?"
	case float64:
		return fmt3(x)
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case map[string]any:
		keys := sortedKeys(x)
		for i, k := range keys {
			keys[i] = k + "=" + show(x[k])
		}
		return strings.Join(keys, ", ")
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = show(e)
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func heading(r record) string {
	kind := r.d.Kind
	if kind == "" {
		kind = "?"
	}
	return fmt.Sprintf("%s [%s] at %s", r.d.ID, kind, r.d.At.UTC().Format(time.RFC3339))
}

// givenBack is the phrase each relief outcome ends on, after its act clause.
var givenBack = map[string]func(r record) string{
	"relief.expired": func(record) string { return "the change expired and was given back" },
	// The loop releases a change only when the runtime no longer lists its
	// workload (outcome.go, verify); easing pressure gives nothing back.
	"relief.release": func(record) string {
		return "the runtime no longer listed the workload and the change was given back"
	},
	"relief.floored": func(record) string {
		return "the workload's floor rose above the cap and the change was given back"
	},
	"relief.manual": func(record) string { return "an operator undid the change" },
	"relief.gone":   func(record) string { return "the workload left; there was nothing to give back" },
	"relief.stuck": func(r record) string {
		return "giving the change back failed while " + r.v("inputs", "while") + ": " + r.v("inputs", "error")
	},
	"relief.not_applied": func(r record) string {
		return "the change never took effect: " + r.v("answer", "not_applied") + " still read as before once settle had passed"
	},
}

// sentence is the kind's template over r; cites holds the held ancestors by id.
func sentence(r record, cites map[string]record) string {
	A := func(path ...string) string { return r.v(append([]string{"answer"}, path...)...) }
	I := func(path ...string) string { return r.v(append([]string{"inputs"}, path...)...) }
	has := func(path ...string) bool { x, ok := r.get(path...); return ok && x != nil }

	if phrase, ok := givenBack[r.d.Kind]; ok {
		act := r.str("inputs", "act")
		if r.d.Kind == "relief.not_applied" {
			act = r.str("inputs", "entry", "decision_id")
		}
		return actClause(act, r.v("inputs", "entry", "subject"), cites) + "; " + phrase(r)
	}

	var s strings.Builder
	switch r.d.Kind {
	case "estimate":
		fmt.Fprintf(&s, "estimated %s at %s (confidence %s, %s); sensitivity %s [%s, %s] from %d influences, contributing %s",
			A("target"), A("level"), A("confidence"), A("status"), A("sensitivity"), A("sensitivity_low"),
			A("sensitivity_high"), len(r.list("relationships_read")), A("contributions"))
		if has("answer", "hypothetical") {
			fmt.Fprintf(&s, "; under %s: projected %s", r.v("assumptions"), A("hypothetical", "projected_level"))
		}
		if n := len(r.list("caveats")); n > 0 {
			fmt.Fprintf(&s, "; %d caveats", n)
		}
	case "admit":
		what := r.str("inputs", "request", "workload")
		if what == "" {
			what = "a workload"
		}
		fmt.Fprintf(&s, "%s %s declaring %s cores and %s bytes", verdict(r.val("answer", "accepted")), what,
			I("request", "cpu_cores"), I("request", "memory_bytes"))
		if from := r.str("inputs", "request", "from"); from != "" {
			s.WriteString(" for " + from)
		}
		fmt.Fprintf(&s, ": %s (basis %s", A("reason"), A("basis"))
		if has("answer", "cpu_after") {
			fmt.Fprintf(&s, ", cpu after %s, memory after %s", A("cpu_after"), A("memory_after"))
		}
		s.WriteString(")")
	case "relief.act":
		return reliefAct(r, cites)
	case "relief.verify":
		subject, _, _ := strings.Cut(strings.TrimPrefix(r.d.Question, "did capping "), "'s ")
		s.WriteString(actClause(r.str("inputs", "act"), subject, cites) + "; observed " + I("observed") + "; ")
		switch v := r.str("answer", "verdict"); v {
		case "keep":
			s.WriteString("kept")
		case "revert":
			s.WriteString("reverted")
		default:
			s.WriteString(A("verdict"))
		}
		if c, _ := r.get("inputs", "confounded", "confounded"); c == true {
			s.WriteString(" (confounded: other workloads moved)")
		}
		if changed := r.str("inputs", "intervention", "changed"); changed != "" {
			fmt.Fprintf(&s, "; relationship %s %s", I("intervention", "relationship"), changed)
		}
	case "relief.revert":
		observed := "?"
		if v, ok := cites[r.str("inputs", "verify")]; ok {
			observed = v.v("inputs", "observed")
		}
		fmt.Fprintf(&s, "%s; observed %s; reverted", actClause(r.str("inputs", "act"), r.v("inputs", "entry", "subject"), cites), observed)
		if has("inputs", "restored") {
			fmt.Fprintf(&s, ", restoring %s %s from %s", I("restored", "to"), I("restored", "unit"), I("restored", "act"))
		}
	case "relief.refused":
		if has("inputs", "plan") {
			fmt.Fprintf(&s, "refused to cap %s (to %s %s): %s", I("plan", "subject"), I("plan", "to"), I("plan", "unit"), A("refused"))
		} else {
			s.WriteString("refused to relieve: " + A("refused"))
		}
	case "admission":
		fmt.Fprintf(&s, "%s %s: %s", verdict(r.val("answer", "verdict")), I("offer", "name"), A("reason"))
		for i := range r.list("inputs", "domains") {
			l := func(f string) string { return I("domains", strconv.Itoa(i), f) }
			pass := "fails"
			if p, _ := r.get("inputs", "domains", strconv.Itoa(i), "Pass"); p == true {
				pass = "passes"
			}
			pred := "MapPred"
			if l("Basis") == "usage" || (l("Basis") != "map" && !has("inputs", "domains", strconv.Itoa(i), "MapPred")) {
				pred = "UsagePred"
			}
			fmt.Fprintf(&s, "; %s %s: pressure %s + share %s → %s (%s) against exit %s",
				l("Domain"), pass, l("Pressure"), l("Share"), l(pred), l("Basis"), l("Exit"))
		}
	case "admission.outcome":
		fmt.Fprintf(&s, "%s: %s", I("name"), A("outcome"))
		if reason := r.str("answer", "reason"); reason != "" {
			fmt.Fprintf(&s, " (%s)", reason)
		}
		for i := range r.list("inputs", "lines") {
			l := func(f string) string { return I("lines", strconv.Itoa(i), f) }
			fmt.Fprintf(&s, "; %s rose %s against map %s / usage %s", l("domain"), l("observed_rise"), l("map_rise"), l("usage_rise"))
			if b, _ := r.get("inputs", "lines", strconv.Itoa(i), "breached"); b == true {
				s.WriteString(", breached")
			}
		}
	case "review":
		fmt.Fprintf(&s, "reviewed candidate %s: %s by %s at tier %s (%s)", I("candidate", "CandidateID"), A("action"), A("by"), A("tier"), A("level"))
		if a, _ := r.get("answer", "applied"); a == true {
			s.WriteString(", applied")
		}
		if model := r.str("answer", "model"); model != "" {
			s.WriteString(", model " + model)
		}
		if a, _ := r.get("answer", "association"); a == true {
			s.WriteString(", as an association")
		}
	case "cost":
		fmt.Fprintf(&s, "cost of %s on %s: resource %s, latency %s, sensitivities %s / %s, confidence %s",
			A("task_type"), A("node"), A("resource_cost"), A("latency_estimate"), A("resource_sensitivity"),
			A("pressure_sensitivity"), A("confidence"))
	default:
		answer, _ := r.get("answer")
		fmt.Fprintf(&s, "decided %s: %s", r.d.Question, show(answer))
		if in, _ := r.get("inputs"); in != nil {
			if m, _ := in.(map[string]any); len(m) > 0 {
				s.WriteString("; inputs: " + strings.Join(sortedKeys(m), ", "))
			}
		}
	}
	return s.String()
}

// verdict is accepted or declined, from an admit's bool or an admission's word.
func verdict(v any) string {
	switch v {
	case true, "accept":
		return "accepted"
	case false, "decline":
		return "declined"
	}
	return show(v)
}

// actClause is the act's own sentence when it is held, else the change named by
// its subject, saying the act is no longer held when there was one.
func actClause(act, subject string, cites map[string]record) string {
	if a, ok := cites[act]; ok && a.d.Kind == "relief.act" {
		return reliefAct(a, cites)
	}
	if act == "" {
		return "the change to " + subject
	}
	return fmt.Sprintf("the change to %s (act %s no longer held)", subject, act)
}

func reliefAct(r record, cites map[string]record) string {
	I := func(path ...string) string { return r.v(append([]string{"inputs"}, path...)...) }
	A := func(path ...string) string { return r.v(append([]string{"answer"}, path...)...) }
	var basis string
	switch r.str("inputs", "plan", "basis") {
	case "map":
		basis = mapBasis(r, cites)
	case "usage":
		basis = "the workload's share of usage (effect " + I("plan", "effect") + ")"
	default:
		basis = "the " + I("plan", "basis") + " basis"
	}
	return fmt.Sprintf("capped %s (%s to %s %s) because %s stayed above %s for %s (at %s); predicted a drop of %s from %s",
		I("plan", "subject"), A("action"), A("to"), A("unit"), I("notice", "relieves"), I("notice", "enter"),
		I("notice", "enter_for"), I("notice", "pressure"), A("predicted"), basis)
}

// mapBasis names the relationship the act's estimate read from the capped
// workload's usage. The act does not record the usage property's id, so the
// relationship is the one whose source is a property of the subject.
func mapBasis(r record, cites map[string]record) string {
	id := r.str("inputs", "plan", "estimate_id")
	e, ok := cites[id]
	if !ok {
		return "the map (estimate " + id + " no longer held)"
	}
	suffix := "@" + r.str("inputs", "plan", "subject")
	for i := range e.list("relationships_read") {
		at := func(f string) []string { return []string{"relationships_read", strconv.Itoa(i), f} }
		if !strings.HasSuffix(e.str(at("from")...), suffix) {
			continue
		}
		measured := "none"
		if _, ok := e.num(at("measured")...); ok {
			measured = e.v(at("measured")...)
		}
		return fmt.Sprintf("the map (%s, %s, measured %s)", e.v(at("id")...), e.v(at("provenance")...), measured)
	}
	return "the map (estimate " + id + " read no relationship from " + r.str("inputs", "plan", "subject") + ")"
}
