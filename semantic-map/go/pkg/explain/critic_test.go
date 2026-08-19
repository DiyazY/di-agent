package explain

import (
	"strings"
	"testing"
)

func TestParseCriticVerdict_Approval(t *testing.T) {
	v, err := parseCriticVerdict(`{"approved":true,"issues":[],"suggested_revision":""}`)
	if err != nil {
		t.Fatalf("parseCriticVerdict: %v", err)
	}
	if !v.Approved || len(v.Issues) != 0 {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseCriticVerdict_Rejection(t *testing.T) {
	v, err := parseCriticVerdict(`{"approved":false,"issues":["P7 is CE→MU, not an RC edge"],"suggested_revision":"Recompute using RC-destination edges only."}`)
	if err != nil {
		t.Fatalf("parseCriticVerdict: %v", err)
	}
	if v.Approved {
		t.Error("expected rejection")
	}
	if len(v.Issues) != 1 || !strings.Contains(v.Issues[0], "CE→MU") {
		t.Errorf("unexpected issues: %v", v.Issues)
	}
}

func TestParseCriticVerdict_StripsFences(t *testing.T) {
	v, err := parseCriticVerdict("```json\n{\"approved\":true}\n```")
	if err != nil {
		t.Fatalf("parseCriticVerdict: %v", err)
	}
	if !v.Approved {
		t.Error("expected approval")
	}
}

func TestParseCriticVerdict_TruncatesIssueList(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"approved":false,"issues":[`)
	for i := 0; i < MaxCriticIssues+3; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"issue"`)
	}
	b.WriteString(`]}`)
	v, err := parseCriticVerdict(b.String())
	if err != nil {
		t.Fatalf("parseCriticVerdict: %v", err)
	}
	if len(v.Issues) != MaxCriticIssues {
		t.Errorf("expected truncation to %d issues; got %d", MaxCriticIssues, len(v.Issues))
	}
}

// A rejection with nothing actionable is upgraded to an approval — burning a
// revision round on "make it better" helps nobody.
func TestParseCriticVerdict_EmptyRejectionBecomesApproval(t *testing.T) {
	v, err := parseCriticVerdict(`{"approved":false,"issues":[],"suggested_revision":""}`)
	if err != nil {
		t.Fatalf("parseCriticVerdict: %v", err)
	}
	if !v.Approved {
		t.Error("expected an unactionable rejection to be upgraded to approval")
	}
}

func TestParseCriticVerdict_RejectsNonJSON(t *testing.T) {
	if _, err := parseCriticVerdict("the answer looks fine to me"); err == nil {
		t.Fatal("expected an error for prose-only critic output")
	}
}

func TestBuildCriticPrompt_IncludesQuestionAnswerAndCitations(t *testing.T) {
	candidate := &ExplainResponse{
		Answer:     "P10 dominates.",
		Confidence: "high",
		Citations: []Citation{
			{Kind: "edge", ID: "P10", Established: ptr(0.645), EMAWeight: 0.62, Confidence: 0.6, NObservations: 15},
		},
	}
	got := buildCriticPrompt("Why is cost high?", candidate, "EVIDENCE: get_cost → 0.035")

	for _, want := range []string{
		"Why is cost high?",
		"P10 dominates.",
		"kind=edge id=P10",
		"established=0.6450",
		"n_obs=15",
		"SELF-REPORTED CONFIDENCE: high",
		"EVIDENCE: get_cost → 0.035",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("critic prompt missing %q\n---\n%s", want, got)
		}
	}
}

func TestBuildCriticPrompt_IncludesProposalWhenPresent(t *testing.T) {
	candidate := &ExplainResponse{
		Answer:     "Deprecate P7.",
		Confidence: "medium",
		Proposal: &Proposal{
			Kind:      "deprecate",
			Endpoint:  "POST /ontology/deprecate",
			Payload:   map[string]any{"proposition_id": "P7", "reason": "stale"},
			Rationale: "Zero observations across six runs.",
		},
	}
	got := buildCriticPrompt("Should I deprecate P7?", candidate, "")
	if !strings.Contains(got, "PROPOSED ACTION") || !strings.Contains(got, "P7") {
		t.Errorf("critic prompt should carry the proposal; got:\n%s", got)
	}
}

func TestBuildCriticPrompt_HandlesNoCitations(t *testing.T) {
	candidate := &ExplainResponse{Answer: "Not enough evidence.", Confidence: "low"}
	got := buildCriticPrompt("Why?", candidate, "")
	if !strings.Contains(got, "(none)") {
		t.Errorf("expected an explicit '(none)' marker for the empty citation set; got:\n%s", got)
	}
}

func TestFormatCriticVerdictForLLM_ApprovalIsEmpty(t *testing.T) {
	if got := FormatCriticVerdictForLLM(&CriticVerdict{Approved: true}); got != "" {
		t.Errorf("approval should produce no revision prompt; got %q", got)
	}
	if got := FormatCriticVerdictForLLM(nil); got != "" {
		t.Errorf("nil verdict should produce no revision prompt; got %q", got)
	}
}

func TestFormatCriticVerdictForLLM_RejectionCarriesIssuesAndRevision(t *testing.T) {
	got := FormatCriticVerdictForLLM(&CriticVerdict{
		Approved:          false,
		Issues:            []string{"wrong direction sign on P10"},
		SuggestedRevision: "Respect each edge's direction.",
	})
	if !strings.Contains(got, "wrong direction sign on P10") {
		t.Error("revision prompt should list the critic's issues")
	}
	if !strings.Contains(got, "Respect each edge's direction.") {
		t.Error("revision prompt should carry the suggested revision")
	}
	if !strings.Contains(got, "match live graph state") {
		t.Error("revision prompt should re-assert the grounding requirement")
	}
}

// ptr is a local helper for the pointer-valued strength layers, which are pointers so
// that "no measurement" is distinguishable from "measured as zero".
func ptr(f float64) *float64 { return &f }
