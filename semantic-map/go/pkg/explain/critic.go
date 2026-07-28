package explain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxCriticIssues caps how many issues a critic verdict may carry into the
// revision prompt. A critic that finds twenty problems is usually pattern-
// matching rather than reasoning; the answering agent can only act on a few
// per round anyway.
const MaxCriticIssues = 4

// parseCriticVerdict extracts a CriticVerdict from the critic agent's
// message. Same tolerance for fences and stray prose as parsePlan.
func parseCriticVerdict(content string) (*CriticVerdict, error) {
	content = stripJSONEnvelope(content)
	if content == "" {
		return nil, errors.New("critic returned an empty message")
	}
	var v CriticVerdict
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return nil, fmt.Errorf("critic output is not valid JSON: %w", err)
	}
	if len(v.Issues) > MaxCriticIssues {
		v.Issues = v.Issues[:MaxCriticIssues]
	}
	// A rejection with no stated issue is unusable — the answering agent has
	// nothing to act on. Treat it as an approval rather than burning a
	// revision round on "make it better".
	if !v.Approved && len(v.Issues) == 0 && strings.TrimSpace(v.SuggestedRevision) == "" {
		v.Approved = true
	}
	return &v, nil
}

// buildCriticPrompt assembles the user message the critic reviews: the
// operator's original question, the candidate answer with its citations, and
// (when planning ran) the evidence the answer was built from.
//
// The critic is deliberately NOT given tools. Giving it tool access would let
// it fetch different data than the answering agent saw, producing critiques
// the answering agent cannot act on ("you missed edge X" when X was never in
// the evidence bundle). Reviewing the same evidence keeps the loop closed.
func buildCriticPrompt(question string, candidate *ExplainResponse, evidence string) string {
	var b strings.Builder

	b.WriteString("OPERATOR'S QUESTION:\n")
	b.WriteString(question)
	b.WriteString("\n\nCANDIDATE ANSWER:\n")
	b.WriteString(candidate.Answer)

	b.WriteString("\n\nCITATIONS THE ANSWER MADE (already verified against live graph state):\n")
	if len(candidate.Citations) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, c := range candidate.Citations {
			fmt.Fprintf(&b, "- kind=%s id=%s", c.Kind, c.ID)
			if c.PriorWeight != 0 {
				fmt.Fprintf(&b, " prior=%.4f", c.PriorWeight)
			}
			if c.EMAWeight != 0 {
				fmt.Fprintf(&b, " ema=%.4f", c.EMAWeight)
			}
			if c.Confidence != 0 {
				fmt.Fprintf(&b, " confidence=%.4f", c.Confidence)
			}
			if c.NObservations != 0 {
				fmt.Fprintf(&b, " n_obs=%d", c.NObservations)
			}
			if c.Trust != 0 {
				fmt.Fprintf(&b, " trust=%.4f", c.Trust)
			}
			b.WriteByte('\n')
		}
	}

	fmt.Fprintf(&b, "\nSELF-REPORTED CONFIDENCE: %s\n", candidate.Confidence)

	if candidate.Proposal != nil {
		payload, _ := json.Marshal(candidate.Proposal.Payload)
		fmt.Fprintf(&b, "\nPROPOSED ACTION: kind=%s endpoint=%s payload=%s\nRationale: %s\n",
			candidate.Proposal.Kind, candidate.Proposal.Endpoint, string(payload), candidate.Proposal.Rationale)
	}

	if strings.TrimSpace(evidence) != "" {
		b.WriteString("\nEVIDENCE THE ANSWER WAS BUILT FROM:\n")
		b.WriteString(evidence)
		b.WriteByte('\n')
	}

	b.WriteString("\nReview the answer and return your verdict as JSON.")
	return b.String()
}

// FormatCriticVerdictForLLM renders a rejection into the revision prompt the
// answering agent receives. Mirrors FormatIssuesForLLM's shape so both
// critique sources read the same way in the transcript.
func FormatCriticVerdictForLLM(v *CriticVerdict) string {
	if v == nil || v.Approved {
		return ""
	}
	var b strings.Builder
	b.WriteString("An independent reviewer rejected the previous response:\n")
	for _, issue := range v.Issues {
		b.WriteString("- ")
		b.WriteString(issue)
		b.WriteByte('\n')
	}
	if rev := strings.TrimSpace(v.SuggestedRevision); rev != "" {
		b.WriteString("\nRequired revision: ")
		b.WriteString(rev)
		b.WriteByte('\n')
	}
	b.WriteString("\nProduce a corrected response. Every cited value must still match live graph state — " +
		"re-fetch with tools if you need to confirm anything.")
	return b.String()
}
