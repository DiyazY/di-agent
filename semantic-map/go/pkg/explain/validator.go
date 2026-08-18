package explain

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/statemap"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ValidationResult reports the outcome of one deterministic validation pass
// over an ExplainResponse. IsValid=true means every citation exists in the
// live graph and every value matches within Epsilon. IsValid=false comes with
// a bounded slice of Issues the reflection loop hands back to the LLM.
type ValidationResult struct {
	IsValid bool
	Issues  []string
}

// Epsilon governs float equality between the LLM's cited value and the live
// graph. Tight enough to catch fabrication, loose enough to survive rounding
// in the LLM's JSON output. 1e-3 corresponds to the precision the response
// schema encourages (three decimal places in most examples).
const Epsilon = 1e-3

// Validate checks a candidate response against a live SemanticMapReader.
//
// Deterministic checks, in order:
//  1. Every Citation.ID exists as the right kind (edge/proposition/peer/construct/event).
//  2. Every numeric field (ema_weight, prior_weight, confidence, n_observations,
//     trust) that the citation carries matches the live value.
//  3. Deprecated edges are not cited as active evidence (kind == "edge" &&
//     deprecated=false is required); operators may reference them via
//     kind == "event".
//  4. If the answer mentions a proposition ID, that ID must appear in citations.
//  5. If the response carries a Proposal, its Endpoint is a known route and
//     its Payload has the required keys for that endpoint.
//
// Non-goals: we do not judge whether the *answer text* is factually correct,
// only whether its structured claims match the graph. Semantic correctness is
// what the LLM is for; consistency with graph state is what we can verify.
func Validate(reader SemanticMapReader, resp *ExplainResponse) ValidationResult {
	if resp == nil {
		return ValidationResult{IsValid: false, Issues: []string{"response is nil"}}
	}
	var issues []string

	edgesByPID, propsByID, peersByID, constructIDs := indexGraph(reader)

	// Track which proposition IDs the LLM actually cited, so we can check the
	// answer for uncited proposition references below.
	citedProps := make(map[string]struct{}, len(resp.Citations))

	for i, c := range resp.Citations {
		prefix := fmt.Sprintf("citations[%d] (kind=%s, id=%s)", i, c.Kind, c.ID)
		switch c.Kind {
		case "edge":
			edge, ok := edgesByPID[c.ID]
			if !ok {
				issues = append(issues, prefix+": no edge with this proposition_id exists")
				continue
			}
			if edge.Deprecated && !c.Deprecated {
				issues = append(issues, prefix+": edge is deprecated but citation does not mark it so")
			}
			if c.EMAWeight != 0 && !floatMatch(c.EMAWeight, edge.EMAWeight) {
				issues = append(issues, fmt.Sprintf("%s: ema_weight %.4f ≠ live %.4f", prefix, c.EMAWeight, edge.EMAWeight))
			}
			if c.PriorWeight != 0 && !floatMatch(c.PriorWeight, edge.PriorWeight) {
				issues = append(issues, fmt.Sprintf("%s: prior_weight %.4f ≠ live %.4f", prefix, c.PriorWeight, edge.PriorWeight))
			}
			if c.Confidence != 0 && !floatMatch(c.Confidence, edge.Confidence) {
				issues = append(issues, fmt.Sprintf("%s: confidence %.4f ≠ live %.4f", prefix, c.Confidence, edge.Confidence))
			}
			if c.NObservations != 0 && c.NObservations != edge.NObservations {
				issues = append(issues, fmt.Sprintf("%s: n_observations %d ≠ live %d", prefix, c.NObservations, edge.NObservations))
			}
			citedProps[c.ID] = struct{}{}

		case "proposition":
			p, ok := propsByID[c.ID]
			if !ok {
				issues = append(issues, prefix+": no proposition with this ID exists")
				continue
			}
			if c.PriorWeight != 0 && !floatMatch(c.PriorWeight, p.PriorStrength) {
				issues = append(issues, fmt.Sprintf("%s: prior_weight %.4f ≠ prior_strength %.4f", prefix, c.PriorWeight, p.PriorStrength))
			}
			citedProps[c.ID] = struct{}{}

		case "peer":
			pd, ok := peersByID[c.ID]
			if !ok {
				issues = append(issues, prefix+": no peer registered with this ID")
				continue
			}
			if c.Trust != 0 && !floatMatch(c.Trust, pd.Trust) {
				issues = append(issues, fmt.Sprintf("%s: trust %.4f ≠ live %.4f", prefix, c.Trust, pd.Trust))
			}

		case "construct":
			if _, ok := constructIDs[c.ID]; !ok {
				issues = append(issues, prefix+": no construct with this ID exists")
			}

		case "property":
			// A property citation is checked against the state model — the model the
			// agent reasons from. This is what makes the surface's claim hold: an answer
			// about what the system is doing is verified against the same values a
			// decision would have used, not against a backbone no decision reads.
			sm := reader.State()
			if sm == nil {
				issues = append(issues, prefix+
					": cites a property but this agent has no state model to check it against")
				break
			}
			p, ok := sm.Property(c.ID)
			if !ok {
				issues = append(issues, prefix+": no property with this ID is in the map")
				break
			}
			// Zero means "not cited": a citation that omits a value is making no
			// numeric claim, and inventing one to check would reject honest answers.
			if c.Value != 0 && !floatMatch(c.Value, p.Value) {
				issues = append(issues, fmt.Sprintf("%s: cited value %.4f but the map holds %.4f",
					prefix, c.Value, p.Value))
			}
			if c.Confidence != 0 && !floatMatch(c.Confidence, p.Confidence) {
				issues = append(issues, fmt.Sprintf("%s: cited confidence %.4f but the map holds %.4f",
					prefix, c.Confidence, p.Confidence))
			}
			if p.Status == statemap.Retired {
				issues = append(issues, prefix+
					": cites a retired property as current state; retired properties may be "+
					"referenced as history, not as what the system is doing now")
			}

		case "event":
			// Events are point-in-time; we can't cheaply verify without an
			// audit-log scan. We at least check the timestamp parses. When the
			// event points at a proposition ID we treat it as citing that
			// proposition — a historical event about P7 IS a citation of P7.
			if c.Timestamp == "" {
				issues = append(issues, prefix+": event citations require a timestamp (RFC3339)")
			} else if _, err := time.Parse(time.RFC3339, c.Timestamp); err != nil {
				issues = append(issues, prefix+": timestamp is not RFC3339")
			}
			if _, isProp := propsByID[c.ID]; isProp {
				citedProps[c.ID] = struct{}{}
			}

		default:
			issues = append(issues, prefix+": unknown citation kind (expected edge/proposition/peer/event/construct)")
		}
	}

	// Rule 4: if answer text names a P<n> proposition, it must be cited.
	for _, pid := range findPropositionsInText(resp.Answer, propsByID) {
		if _, ok := citedProps[pid]; !ok {
			issues = append(issues, fmt.Sprintf("answer references %s but %s is not in citations", pid, pid))
		}
	}

	// Rule 5: proposal endpoint + payload sanity.
	if resp.Proposal != nil {
		if err := validateProposal(resp.Proposal); err != nil {
			issues = append(issues, "proposal: "+err.Error())
		}
	}

	return ValidationResult{IsValid: len(issues) == 0, Issues: issues}
}

// FormatIssuesForLLM returns a compact string the reflection loop can hand
// back to the LLM as a critique. Bounded to the first 5 issues to keep the
// context small.
func FormatIssuesForLLM(issues []string) string {
	if len(issues) == 0 {
		return ""
	}
	max := 5
	if len(issues) < max {
		max = len(issues)
	}
	var b strings.Builder
	b.WriteString("The previous response failed citation validation:\n")
	for i := 0; i < max; i++ {
		b.WriteString("- ")
		b.WriteString(issues[i])
		b.WriteByte('\n')
	}
	if len(issues) > max {
		b.WriteString(fmt.Sprintf("- (%d more issues omitted)\n", len(issues)-max))
	}
	b.WriteString("Revise the response so every cited value matches live graph state. Fetch fresh data with tools if you're unsure.")
	return b.String()
}

// ── helpers ─────────────────────────────────────────────────────────────────

func floatMatch(a, b float64) bool {
	return math.Abs(a-b) <= Epsilon
}

func indexGraph(reader SemanticMapReader) (
	edges map[string]*types.EdgeDescriptor,
	props map[string]*types.Proposition,
	peersByID map[string]*peers.Descriptor,
	constructs map[string]struct{},
) {
	edges = make(map[string]*types.EdgeDescriptor)
	props = make(map[string]*types.Proposition)
	peersByID = make(map[string]*peers.Descriptor)
	constructs = make(map[string]struct{})

	if all, err := reader.AllEdges(); err == nil {
		for _, e := range all {
			edges[e.PropositionID] = e
		}
	}
	if allProps, err := reader.Propositions(); err == nil {
		for _, p := range allProps {
			props[p.PropositionID] = p
		}
	}
	if reg := reader.Peers(); reg != nil {
		if list, err := reg.List(); err == nil {
			for _, pd := range list {
				peersByID[pd.ID] = pd
			}
		}
	}
	if cs, err := reader.Constructs(); err == nil {
		for _, c := range cs {
			constructs[c.ConstructID] = struct{}{}
		}
	}
	return
}

// findPropositionsInText scans free text for tokens like "P10", "P1)", "P10:"
// and returns the canonical IDs found. We do not use a regex here to keep the
// import surface tight; a simple scan is enough for the P<digits> shape.
func findPropositionsInText(text string, propsByID map[string]*types.Proposition) []string {
	if text == "" || len(propsByID) == 0 {
		return nil
	}
	var out []string
	seen := make(map[string]struct{}, 8)
	for i := 0; i < len(text); i++ {
		if text[i] != 'P' {
			continue
		}
		// Must be at a word boundary — previous char is non-alphanumeric or start.
		if i > 0 {
			prev := text[i-1]
			if isAlnum(prev) {
				continue
			}
		}
		j := i + 1
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		if j == i+1 {
			continue // no digits after P
		}
		id := text[i:j]
		if _, exists := propsByID[id]; !exists {
			continue // not a real proposition ID; ignore
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		i = j - 1
	}
	return out
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

var knownProposalEndpoints = map[string][]string{
	"deprecate": {"proposition_id", "reason"},
	"tune":      {"intent"},
	"reset":     {"from", "to"},
	"strength":  {"proposition_id", "strength"},
}

func validateProposal(p *Proposal) error {
	required, ok := knownProposalEndpoints[p.Kind]
	if !ok {
		return fmt.Errorf("unknown proposal kind %q (expected one of: deprecate, tune, reset, strength)", p.Kind)
	}
	if p.Payload == nil {
		return fmt.Errorf("proposal payload is empty; required keys for %q: %v", p.Kind, required)
	}
	for _, key := range required {
		if _, ok := p.Payload[key]; !ok {
			return fmt.Errorf("proposal payload missing required key %q for kind %q", key, p.Kind)
		}
	}
	if p.Rationale == "" {
		return fmt.Errorf("proposal rationale must not be empty")
	}
	return nil
}
