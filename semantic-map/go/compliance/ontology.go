package compliance

import (
	"errors"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

// OntologyFactory builds a fresh OntologyContract for one compliance subtest.
// Each subtest receives an independent instance so state changes in one test
// cannot leak into another.
type OntologyFactory func(t *testing.T) contracts.OntologyContract

// RunOntologyCompliance verifies the behavioral guarantees of OntologyContract:
//
//   - Non-empty and internally consistent: at least one construct and one
//     proposition, and every proposition's endpoints are declared constructs.
//     The suite deliberately does not assert a size, because the graph's scope
//     comes from whatever domain specification the implementation loaded.
//   - Uniqueness: every proposition has a unique PropositionID.
//   - Relationships is a filter over Propositions.
//   - ValidateProposition is pure (no state change).
//   - AddValidatedProposition rejects contradicting edges.
func RunOntologyCompliance(t *testing.T, factory OntologyFactory) {
	t.Helper()

	// ── Non-empty and internally consistent ───────────────────────────────────

	t.Run("IsNotEmpty", func(t *testing.T) {
		o := factory(t)
		cs, err := o.Constructs()
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) == 0 {
			t.Error("ontology exposes no constructs")
		}
		ps, err := o.Propositions()
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) == 0 {
			t.Error("ontology exposes no propositions")
		}
	})

	t.Run("PropositionEndpointsAreDeclaredConstructs", func(t *testing.T) {
		o := factory(t)
		cs, err := o.Constructs()
		if err != nil {
			t.Fatal(err)
		}
		declared := make(map[string]bool, len(cs))
		for _, c := range cs {
			declared[c.ConstructID] = true
		}
		ps, err := o.Propositions()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if !declared[p.FromConstruct] {
				t.Errorf("proposition %s starts at undeclared construct %q", p.PropositionID, p.FromConstruct)
			}
			if !declared[p.ToConstruct] {
				t.Errorf("proposition %s ends at undeclared construct %q", p.PropositionID, p.ToConstruct)
			}
		}
	})

	t.Run("ConstructIDsAreUnique", func(t *testing.T) {
		o := factory(t)
		cs, _ := o.Constructs()
		seen := make(map[string]bool, len(cs))
		for _, c := range cs {
			if seen[c.ConstructID] {
				t.Errorf("duplicate ConstructID %q", c.ConstructID)
			}
			seen[c.ConstructID] = true
		}
	})

	t.Run("PropositionIDsAreUnique", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		seen := make(map[string]bool, len(ps))
		for _, p := range ps {
			if seen[p.PropositionID] {
				t.Errorf("duplicate PropositionID %q", p.PropositionID)
			}
			seen[p.PropositionID] = true
		}
	})

	// ── Relationships ─────────────────────────────────────────────────────────

	t.Run("RelationshipsIsSubsetOfPropositions", func(t *testing.T) {
		o := factory(t)
		cs, _ := o.Constructs()
		if len(cs) == 0 {
			t.Skip("no constructs to query")
		}
		ps, _ := o.Propositions()
		propSet := make(map[string]bool, len(ps))
		for _, p := range ps {
			propSet[p.PropositionID] = true
		}
		rels, err := o.Relationships(cs[0].ConstructID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rels {
			if !propSet[r.PropositionID] {
				t.Errorf("Relationships(%q) returned proposition %q not in Propositions()",
					cs[0].ConstructID, r.PropositionID)
			}
		}
	})

	t.Run("RelationshipsUnknownConstructReturnsEmpty", func(t *testing.T) {
		o := factory(t)
		rels, err := o.Relationships("definitely-not-a-real-construct-id")
		if err != nil {
			t.Fatal(err)
		}
		if len(rels) != 0 {
			t.Errorf("expected empty for unknown construct; got %d relationships", len(rels))
		}
	})

	t.Run("RelationshipsFiltersByConstruct", func(t *testing.T) {
		o := factory(t)
		cs, _ := o.Constructs()
		if len(cs) == 0 {
			t.Skip("no constructs to query")
		}
		cid := cs[0].ConstructID
		rels, _ := o.Relationships(cid)
		for _, r := range rels {
			if r.FromConstruct != cid && r.ToConstruct != cid {
				t.Errorf("Relationships(%q) returned %q with from=%q to=%q — neither matches",
					cid, r.PropositionID, r.FromConstruct, r.ToConstruct)
			}
		}
	})

	// ── ValidateProposition purity ───────────────────────────────────────────

	t.Run("ValidatePropositionIsPure", func(t *testing.T) {
		o := factory(t)
		before, _ := o.Propositions()
		_, err := o.ValidateProposition("X", "Y", types.Positive)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := o.Propositions()
		if len(before) != len(after) {
			t.Errorf("ValidateProposition must not modify state: %d → %d propositions",
				len(before), len(after))
		}
	})

	// ── AddValidatedProposition ───────────────────────────────────────────────

	t.Run("AddContradictingPropositionFails", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no existing propositions to contradict")
		}
		existing := ps[0]
		opposite := types.Positive
		if existing.Direction == types.Positive {
			opposite = types.Negative
		}
		bad := &types.Proposition{
			PropositionID: "P-bad-contradiction",
			FromConstruct: existing.FromConstruct,
			ToConstruct:   existing.ToConstruct,
			Direction:     opposite,
			PriorStrength: 0.5,
		}
		if err := o.AddValidatedProposition(bad); err == nil {
			t.Errorf("AddValidatedProposition must reject contradicting edge %s→%s (existing %s)",
				bad.FromConstruct, bad.ToConstruct, existing.PropositionID)
		}
	})

	// ── Live ontology: SetPropositionStrength ────────────────────────────────

	t.Run("SetPropositionStrengthUpdatesPrior", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no propositions to mutate")
		}
		target := ps[0].PropositionID
		err := o.SetPropositionStrength(target, 0.999)
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support SetPropositionStrength")
		}
		if err != nil {
			t.Fatal(err)
		}
		after, _ := o.Propositions()
		var found bool
		for _, p := range after {
			if p.PropositionID == target {
				found = true
				if p.PriorStrength != 0.999 {
					t.Errorf("expected PriorStrength=0.999; got %.4f", p.PriorStrength)
				}
			}
		}
		if !found {
			t.Errorf("proposition %q vanished after SetPropositionStrength", target)
		}
	})

	t.Run("SetPropositionStrengthUnknownFails", func(t *testing.T) {
		o := factory(t)
		err := o.SetPropositionStrength("P-never-existed", 0.5)
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support SetPropositionStrength")
		}
		if err == nil {
			t.Error("expected error for unknown propositionID")
		}
	})

	// ── Live ontology: AddConstruct ──────────────────────────────────────────

	t.Run("AddConstructAppearsInConstructs", func(t *testing.T) {
		o := factory(t)
		newC := &types.Construct{
			ConstructID: "TEST-NEW-CONSTRUCT",
			Name:        "Test New Construct",
			Description: "Added by compliance test",
		}
		err := o.AddConstruct(newC)
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support AddConstruct")
		}
		if err != nil {
			t.Fatal(err)
		}
		cs, _ := o.Constructs()
		found := false
		for _, c := range cs {
			if c.ConstructID == "TEST-NEW-CONSTRUCT" {
				found = true
				break
			}
		}
		if !found {
			t.Error("added construct not present in Constructs() after AddConstruct")
		}
	})

	t.Run("AddConstructRejectsDuplicate", func(t *testing.T) {
		o := factory(t)
		cs, _ := o.Constructs()
		if len(cs) == 0 {
			t.Skip("no existing constructs to duplicate")
		}
		dup := &types.Construct{
			ConstructID: cs[0].ConstructID,
			Name:        "Duplicate",
			Description: "should be rejected",
		}
		err := o.AddConstruct(dup)
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support AddConstruct")
		}
		if err == nil {
			t.Errorf("AddConstruct must reject duplicate ConstructID %q", cs[0].ConstructID)
		}
	})

	t.Run("AddConstructNilOrEmptyFails", func(t *testing.T) {
		o := factory(t)
		err := o.AddConstruct(&types.Construct{ConstructID: "", Name: "x"})
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support AddConstruct")
		}
		if err == nil {
			t.Error("AddConstruct must reject empty ConstructID")
		}
	})

	// ── Live ontology: Deprecate ─────────────────────────────────────────────

	t.Run("DeprecateMarksPropositionDeprecated", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no propositions to deprecate")
		}
		target := ps[0].PropositionID
		err := o.Deprecate(target, "compliance test")
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support Deprecate")
		}
		if err != nil {
			t.Fatal(err)
		}
		after, _ := o.Propositions()
		for _, p := range after {
			if p.PropositionID == target {
				if !p.Deprecated {
					t.Errorf("proposition %q should be marked Deprecated", target)
				}
				if p.DeprecatedReason == "" {
					t.Errorf("proposition %q DeprecatedReason should be set", target)
				}
			}
		}
	})

	t.Run("DeprecateIsIdempotent", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no propositions to deprecate")
		}
		target := ps[0].PropositionID
		if err := o.Deprecate(target, "first call"); err != nil {
			if errors.Is(err, contracts.ErrNotImplemented) {
				t.Skip("implementation does not support Deprecate")
			}
			t.Fatal(err)
		}
		if err := o.Deprecate(target, "second call"); err != nil {
			t.Errorf("second Deprecate on same propositionID should be no-op; got %v", err)
		}
	})

	t.Run("DeprecateUnknownFails", func(t *testing.T) {
		o := factory(t)
		err := o.Deprecate("P-never-existed", "test")
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support Deprecate")
		}
		if err == nil {
			t.Error("expected error for unknown propositionID")
		}
	})

	// ── Live ontology: GetHistory ────────────────────────────────────────────

	t.Run("HistoryStartsEmpty", func(t *testing.T) {
		o := factory(t)
		events, err := o.GetHistory(time.Time{})
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support GetHistory")
		}
		if err != nil {
			t.Fatal(err)
		}
		// A fresh ontology may have zero events (bootstrap from constants is
		// not a "mutation" event). We tolerate an empty slice here — what we
		// care about is that subsequent mutations get logged.
		if events == nil {
			t.Error("GetHistory must return non-nil slice (empty is fine)")
		}
	})

	t.Run("HistoryRecordsStrengthChange", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no propositions to mutate")
		}
		err := o.SetPropositionStrength(ps[0].PropositionID, 0.42)
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support SetPropositionStrength")
		}
		if err != nil {
			t.Fatal(err)
		}
		events, _ := o.GetHistory(time.Time{})
		found := false
		for _, e := range events {
			if e.Kind == types.EventPropositionStrengthSet && e.TargetID == ps[0].PropositionID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected EventPropositionStrengthSet for %q in history", ps[0].PropositionID)
		}
	})

	t.Run("HistoryRecordsDeprecation", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no propositions to deprecate")
		}
		err := o.Deprecate(ps[0].PropositionID, "compliance test")
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support Deprecate")
		}
		if err != nil {
			t.Fatal(err)
		}
		events, _ := o.GetHistory(time.Time{})
		found := false
		for _, e := range events {
			if e.Kind == types.EventPropositionDeprecated && e.TargetID == ps[0].PropositionID {
				found = true
				if reason, _ := e.Detail["reason"].(string); reason != "compliance test" {
					t.Errorf("expected reason 'compliance test'; got %q", reason)
				}
			}
		}
		if !found {
			t.Errorf("expected EventPropositionDeprecated for %q in history", ps[0].PropositionID)
		}
	})

	t.Run("RecordTuneAppendsAuditEvent", func(t *testing.T) {
		o := factory(t)
		err := o.RecordTune("prioritize security", "operator:alice", []string{"P1", "P11"})
		if errors.Is(err, contracts.ErrNotImplemented) {
			t.Skip("implementation does not support RecordTune")
		}
		if err != nil {
			t.Fatalf("RecordTune must not error; got %v", err)
		}
		events, _ := o.GetHistory(time.Time{})
		found := false
		for _, e := range events {
			if string(e.Kind) == "operator-tune" {
				found = true
				if e.Detail["intent"] != "prioritize security" {
					t.Errorf("expected intent='prioritize security'; got %v", e.Detail["intent"])
				}
				if e.Actor != "operator:alice" {
					t.Errorf("expected actor='operator:alice'; got %q", e.Actor)
				}
			}
		}
		if !found {
			t.Error("RecordTune must append an 'operator-tune' event visible in GetHistory")
		}
	})

	t.Run("HistorySinceFilter", func(t *testing.T) {
		o := factory(t)
		ps, _ := o.Propositions()
		if len(ps) == 0 {
			t.Skip("no propositions to mutate")
		}
		// Fire a first event, capture the cutoff, then fire a second event.
		if err := o.SetPropositionStrength(ps[0].PropositionID, 0.10); err != nil {
			if errors.Is(err, contracts.ErrNotImplemented) {
				t.Skip("implementation does not support SetPropositionStrength")
			}
			t.Fatal(err)
		}
		// Allow at least one tick of wall-clock to elapse so the `since` filter
		// can discriminate the two events.
		time.Sleep(2 * time.Millisecond)
		cutoff := time.Now()
		time.Sleep(2 * time.Millisecond)
		if len(ps) > 1 {
			if err := o.SetPropositionStrength(ps[1].PropositionID, 0.20); err != nil {
				t.Fatal(err)
			}
		}
		later, _ := o.GetHistory(cutoff)
		for _, e := range later {
			if e.Timestamp.Before(cutoff) {
				t.Errorf("GetHistory(since=cutoff) returned event from before cutoff: %v", e.Timestamp)
			}
		}
	})

	t.Run("AddValidatedPropositionPersists", func(t *testing.T) {
		o := factory(t)
		// Find a construct pair with no existing proposition between them.
		cs, _ := o.Constructs()
		ps, _ := o.Propositions()
		if len(cs) < 2 {
			t.Skip("need ≥2 constructs to add a fresh proposition")
		}
		existingPairs := make(map[string]bool, len(ps))
		for _, p := range ps {
			existingPairs[p.FromConstruct+"→"+p.ToConstruct] = true
		}
		var fromID, toID string
		for i := 0; i < len(cs) && fromID == ""; i++ {
			for j := 0; j < len(cs); j++ {
				if i == j {
					continue
				}
				key := cs[i].ConstructID + "→" + cs[j].ConstructID
				revKey := cs[j].ConstructID + "→" + cs[i].ConstructID
				if !existingPairs[key] && !existingPairs[revKey] {
					fromID, toID = cs[i].ConstructID, cs[j].ConstructID
					break
				}
			}
		}
		if fromID == "" {
			t.Skip("no fresh construct pair available — all pairs already have propositions")
		}
		newProp := &types.Proposition{
			PropositionID: "P-compliance-test-fresh",
			FromConstruct: fromID,
			ToConstruct:   toID,
			Direction:     types.Positive,
			PriorStrength: 0.5,
		}
		if err := o.AddValidatedProposition(newProp); err != nil {
			t.Fatalf("AddValidatedProposition on fresh edge %s→%s failed: %v", fromID, toID, err)
		}
		after, _ := o.Propositions()
		found := false
		for _, p := range after {
			if p.PropositionID == "P-compliance-test-fresh" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("added proposition not present in Propositions() after AddValidatedProposition")
		}
	})
}
