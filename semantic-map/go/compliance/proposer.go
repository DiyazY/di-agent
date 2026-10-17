package compliance

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/types"
)

// ProposerFactory builds a fresh ProposerContract for one compliance subtest.
type ProposerFactory func(t *testing.T) contracts.ProposerContract

// RunProposerCompliance verifies the behavioral guarantees of ProposerContract:
//
//   - Observe is non-fatal — it never returns errors on well-formed input.
//   - GetCandidates returns only Pending entries (Confirmed/Rejected/Deferred
//     candidates live in history but are not "pending").
//   - Reject permanently suppresses a candidate from future GetCandidates calls.
//   - Rejected candidates appear in GetHistory with status Rejected.
//   - Defer moves a candidate out of pending.
//   - GetHistory is a superset of GetCandidates (every pending entry has a
//     history record).
//
// Implementations that do not generate candidates (e.g. DisabledProposer) pass
// every assertion vacuously — this is the correct behavior for a "no proposals"
// implementation.
func RunProposerCompliance(t *testing.T, factory ProposerFactory) {
	t.Helper()

	// feed pushes n strongly-correlated observations across one construct pair.
	// Implementations that mine correlations should generate at least one
	// candidate after this; implementations that don't (DisabledProposer) will
	// return an empty candidate list, which is also valid.
	feed := func(p contracts.ProposerContract) {
		for i := 0; i < 100; i++ {
			x := float64(i)
			y := x * 0.9
			_ = p.Observe("PS", "RC", x, y)
		}
	}

	// ── Observe ───────────────────────────────────────────────────────────────

	t.Run("ObserveDoesNotError", func(t *testing.T) {
		p := factory(t)
		if err := p.Observe("a", "b", 0.5, 0.4); err != nil {
			t.Errorf("Observe must not error on well-formed input; got %v", err)
		}
	})

	t.Run("ObserveDoesNotPanicOnRepeat", func(t *testing.T) {
		p := factory(t)
		for i := 0; i < 50; i++ {
			if err := p.Observe("a", "b", float64(i), float64(i)*0.5); err != nil {
				t.Fatalf("Observe error at iteration %d: %v", i, err)
			}
		}
	})

	t.Run("ObserveConstructDoesNotError", func(t *testing.T) {
		p := factory(t)
		for i := 0; i < 20; i++ {
			if err := p.ObserveConstruct("RC", float64(i)*0.01); err != nil {
				t.Errorf("ObserveConstruct must not error; got %v", err)
			}
			if err := p.ObserveConstruct("PS", float64(i)*0.008); err != nil {
				t.Errorf("ObserveConstruct must not error; got %v", err)
			}
		}
	})

	// ── GetCandidates ─────────────────────────────────────────────────────────

	t.Run("GetCandidatesReturnsOnlyPending", func(t *testing.T) {
		p := factory(t)
		feed(p)
		cs, err := p.GetCandidates()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Status != types.Pending {
				t.Errorf("GetCandidates returned non-pending candidate %q with status %v",
					c.CandidateID, c.Status)
			}
		}
	})

	t.Run("GetCandidatesInitiallyEmpty", func(t *testing.T) {
		p := factory(t)
		cs, err := p.GetCandidates()
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != 0 {
			t.Errorf("expected empty candidates before any Observe; got %d", len(cs))
		}
	})

	// ── Reject ───────────────────────────────────────────────────────────────

	t.Run("RejectSuppressesCandidate", func(t *testing.T) {
		p := factory(t)
		feed(p)
		cs, _ := p.GetCandidates()
		if len(cs) == 0 {
			t.Skip("implementation generated no candidates — vacuously satisfies Reject contract")
		}
		cid := cs[0].CandidateID
		if err := p.Reject(cid, types.CandidateReview{}); err != nil {
			t.Fatal(err)
		}
		after, _ := p.GetCandidates()
		for _, c := range after {
			if c.CandidateID == cid {
				t.Errorf("rejected candidate %q must not appear in GetCandidates", cid)
			}
		}
	})

	t.Run("RejectedAppearsInHistoryWithCorrectStatus", func(t *testing.T) {
		p := factory(t)
		feed(p)
		cs, _ := p.GetCandidates()
		if len(cs) == 0 {
			t.Skip("implementation generated no candidates — history check skipped")
		}
		cid := cs[0].CandidateID
		if err := p.Reject(cid, types.CandidateReview{}); err != nil {
			t.Fatal(err)
		}
		history, err := p.GetHistory()
		if err != nil {
			t.Fatal(err)
		}
		var found *types.CandidateEdge
		for _, h := range history {
			if h.CandidateID == cid {
				found = h
				break
			}
		}
		if found == nil {
			t.Errorf("rejected candidate %q must appear in GetHistory", cid)
			return
		}
		if found.Status != types.Rejected {
			t.Errorf("history entry for %q has status %v; expected Rejected", cid, found.Status)
		}
	})

	// ── Defer ────────────────────────────────────────────────────────────────

	t.Run("DeferMovesCandidateOutOfPending", func(t *testing.T) {
		p := factory(t)
		feed(p)
		cs, _ := p.GetCandidates()
		if len(cs) == 0 {
			t.Skip("implementation generated no candidates — defer check skipped")
		}
		cid := cs[0].CandidateID
		if err := p.Defer(cid, types.CandidateReview{}); err != nil {
			t.Fatal(err)
		}
		after, _ := p.GetCandidates()
		for _, c := range after {
			if c.CandidateID == cid {
				t.Errorf("deferred candidate %q must not appear in GetCandidates", cid)
			}
		}
	})

	// ── GetHistory ───────────────────────────────────────────────────────────

	// feedProperties drives ObserveProperty with two subjects that report identical
	// series and two node-level properties that follow them, so every pairing would
	// correlate perfectly if it were formed: the history shows which ones were.
	feedProperties := func(p contracts.ProposerContract, subjects []string) {
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 120; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%20) / 20
			for _, sub := range subjects {
				_ = p.ObserveProperty(types.PropertyReading{ID: "cpu@" + sub, Subject: sub, Value: x, At: at})
			}
			_ = p.ObserveProperty(types.PropertyReading{ID: "pressure", Value: 0.9*x + 0.05, At: at})
			_ = p.ObserveProperty(types.PropertyReading{ID: "load", Value: 0.8*x + 0.1, At: at})
		}
	}

	t.Run("ObservePropertyFollowsThePairingRule", func(t *testing.T) {
		p := factory(t)
		feedProperties(p, []string{"pod:a", "pod:b"})
		history, err := p.GetHistory()
		if err != nil {
			t.Fatalf("GetHistory: %v", err)
		}
		for _, c := range history {
			fromScoped, toScoped := strings.Contains(c.FromID, "@"), strings.Contains(c.ToID, "@")
			switch {
			case fromScoped && toScoped && !c.Undirected:
				t.Errorf("two subjects make an undirected candidate; %s claims a direction", c.CandidateID)
			case fromScoped && toScoped && c.FromID > c.ToID:
				t.Errorf("an undirected candidate carries its pair in lexicographic order: %s", c.CandidateID)
			case !fromScoped && toScoped:
				t.Errorf("direction must be scoped -> unscoped: %s", c.CandidateID)
			case fromScoped && !toScoped && c.Undirected:
				t.Errorf("a subject -> node candidate is directed: %s", c.CandidateID)
			case !fromScoped && !toScoped && !c.Undirected:
				t.Errorf("two node-level properties make an undirected candidate; %s claims a direction", c.CandidateID)
			case !fromScoped && !toScoped && c.FromID > c.ToID:
				t.Errorf("an undirected candidate carries its pair in lexicographic order: %s", c.CandidateID)
			}
		}
	})

	t.Run("DiscoveryAccountsForWhatIsTested", func(t *testing.T) {
		p := factory(t)
		feedProperties(p, []string{"pod:a", "pod:b"})
		d, err := p.Discovery()
		if err != nil {
			t.Fatalf("Discovery: %v", err)
		}
		if d == nil {
			if history, _ := p.GetHistory(); len(history) != 0 {
				t.Fatal("an implementation that proposes candidates must report what it tests")
			}
			t.Skip("implementation tests no pairs — nothing to account for")
		}
		if d.Budget > 0 && d.TestedPairs+d.IdlePairs > d.Budget {
			t.Errorf("tested %d pairs and held %d idle with a budget of %d", d.TestedPairs, d.IdlePairs, d.Budget)
		}
		if d.TestedPairs > d.EligiblePairs {
			t.Errorf("tested %d pairs of %d eligible", d.TestedPairs, d.EligiblePairs)
		}
		var sum int
		for _, n := range d.TestedByClass {
			sum += n
		}
		if sum != d.TestedPairs {
			t.Errorf("tested by class sums to %d; tested pairs is %d", sum, d.TestedPairs)
		}
		// Four properties, all observed this tick: six pairs, every class among them.
		if d.Properties != 4 || d.EligiblePairs != 6 {
			t.Errorf("properties=%d eligible=%d; want 4 and 6", d.Properties, d.EligiblePairs)
		}
		if d.Budget == 0 {
			for _, class := range []string{types.PairSubjectNode, types.PairNodeNode, types.PairSubjectSubject} {
				if d.TestedByClass[class] == 0 {
					t.Errorf("no %s pair tested with no budget limit", class)
				}
			}
		}
	})

	t.Run("ArithmeticOfOneChartIsNotProposed", func(t *testing.T) {
		p := factory(t)
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 120; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			x := float64(i%20) / 20
			// busy and idle are complements on one chart: arithmetic, not a finding.
			_ = p.ObserveProperty(types.PropertyReading{ID: "busy", Chart: "cpu", Value: x, At: at})
			_ = p.ObserveProperty(types.PropertyReading{ID: "idle", Chart: "cpu", Value: 1 - x, At: at})
		}
		cs, err := p.GetCandidates()
		if err != nil {
			t.Fatal(err)
		}
		if containsCandidate(cs, "busy<->idle") {
			t.Error("two series of one chart that stay perfectly correlated were proposed")
		}
	})

	t.Run("CandidatesClearTheirBar", func(t *testing.T) {
		p := factory(t)
		feedProperties(p, []string{"pod:a", "pod:b"})
		cs, err := p.GetCandidates()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.PBar > 0 && c.PValue > c.PBar {
				t.Errorf("%s pending at p=%.3g over its bar %.3g", c.CandidateID, c.PValue, c.PBar)
			}
		}
	})

	t.Run("UndirectedConfirmNeedsAnEnd", func(t *testing.T) {
		p := factory(t)
		feedProperties(p, []string{"pod:a"})
		pending, _ := p.GetCandidates()
		var c *types.CandidateEdge
		for _, x := range pending {
			if x.Undirected {
				c = x
				break
			}
		}
		if c == nil {
			t.Skip("implementation generated no undirected candidate — orientation check skipped")
		}
		for _, from := range []string{"", "not-an-end"} {
			if _, err := p.Confirm(c.CandidateID, from, types.CandidateReview{}); !errors.Is(err, contracts.ErrBadConfirmation) {
				t.Errorf("Confirm(%q, %q) = %v; want ErrBadConfirmation", c.CandidateID, from, err)
			}
		}
		if still, _ := p.GetCandidates(); !containsCandidate(still, c.CandidateID) {
			t.Fatalf("a refused confirmation must leave %s pending", c.CandidateID)
		}
		prop, err := p.Confirm(c.CandidateID, c.ToID, types.CandidateReview{})
		if err != nil {
			t.Fatalf("Confirm from the second end: %v", err)
		}
		if prop.FromConstruct != c.ToID || prop.ToConstruct != c.FromID {
			t.Errorf("confirmed from %s, got %s -> %s", c.ToID, prop.FromConstruct, prop.ToConstruct)
		}
	})

	t.Run("ForgetDropsBuffersAndCandidates", func(t *testing.T) {
		p := factory(t)
		feedProperties(p, []string{"pod:a"})
		if err := p.Forget("cpu@pod:a"); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		history, _ := p.GetHistory()
		for _, c := range history {
			if c.FromID == "cpu@pod:a" || c.ToID == "cpu@pod:a" {
				t.Errorf("candidate %s survived Forget", c.CandidateID)
			}
		}
		// The buffers are gone too: one more tick cannot re-emit from the old pairs.
		at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
		_ = p.ObserveProperty(types.PropertyReading{ID: "cpu@pod:a", Subject: "pod:a", Value: 0.5, At: at})
		_ = p.ObserveProperty(types.PropertyReading{ID: "pressure", Subject: "", Value: 0.5, At: at})
		pending, _ := p.GetCandidates()
		for _, c := range pending {
			if c.FromID == "cpu@pod:a" {
				t.Errorf("candidate %s re-emitted from buffers Forget should have dropped", c.CandidateID)
			}
		}
	})

	t.Run("HistoryIsSupersetOfPending", func(t *testing.T) {
		p := factory(t)
		feed(p)
		pending, _ := p.GetCandidates()
		history, err := p.GetHistory()
		if err != nil {
			t.Fatal(err)
		}
		historyIDs := make(map[string]bool, len(history))
		for _, h := range history {
			historyIDs[h.CandidateID] = true
		}
		for _, c := range pending {
			if !historyIDs[c.CandidateID] {
				t.Errorf("pending candidate %q missing from GetHistory — history must be a superset",
					c.CandidateID)
			}
		}
	})
}

func containsCandidate(cs []*types.CandidateEdge, id string) bool {
	for _, c := range cs {
		if c.CandidateID == id {
			return true
		}
	}
	return false
}
