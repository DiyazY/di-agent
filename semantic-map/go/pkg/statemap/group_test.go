package statemap

import (
	"slices"
	"strings"
	"testing"
)

var unitRange = [2]float64{0, 1}

func groupMap(t *testing.T) *Map {
	t.Helper()
	m, c := newTestMap(t, Config{ConvergenceObservations: 10, Alpha: 1, AdmitUnknown: true})
	for _, o := range []Observation{
		{ID: "cpu_utilization@pod:pg", Value: 0.2, Subject: "pod:pg"},
		{ID: "memory_utilization@pod:pg", Value: 0.1, Subject: "pod:pg"},
		{ID: "cpu_utilization@pod:redis", Value: 0.1, Subject: "pod:redis"},
		{ID: "memory_utilization@pod:redis", Value: 0.05, Subject: "pod:redis"},
		{ID: "cpu_utilization", Value: 0.5},
		{ID: "cpu_contention_ratio", Value: 0.3},
	} {
		o.At, o.Range, o.Unit = c.now(), &unitRange, "fraction"
		if err := m.Record(o); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func value(t *testing.T, m *Map, id string) float64 {
	t.Helper()
	p, ok := m.Property(id)
	if !ok {
		t.Fatalf("no property %s", id)
	}
	return p.Value
}

// TestAGroupIsWhatItsMembersUse: a group of workloads — the storage pods, say — uses
// the sum of what its members use, and the map holds that as the group's own derived
// properties, recomputed from the members and never stored.
func TestAGroupIsWhatItsMembersUse(t *testing.T) {
	m := groupMap(t)
	if err := m.DeclareGroup("group:storage", []string{"pod:pg", "pod:redis"}, map[string]string{"group": "storage"}); err != nil {
		t.Fatal(err)
	}
	if v := value(t, m, "cpu_utilization@group:storage"); !groupNearly(v, 0.3) {
		t.Fatalf("group CPU %v; want 0.2 + 0.1", v)
	}
	if v := value(t, m, "memory_utilization@group:storage"); !groupNearly(v, 0.15) {
		t.Fatalf("group memory %v; want 0.1 + 0.05", v)
	}
	if got := m.GroupMembers("group:storage"); !slices.Equal(got, []string{"pod:pg", "pod:redis"}) {
		t.Fatalf("members %v", got)
	}
	// Membership follows the system: a member that leaves the group leaves its sum.
	if err := m.DeclareGroup("group:storage", []string{"pod:pg"}, nil); err != nil {
		t.Fatal(err)
	}
	if v := value(t, m, "cpu_utilization@group:storage"); !groupNearly(v, 0.2) {
		t.Fatalf("group CPU %v after redis left; want 0.2", v)
	}
}

// TestOnlyAGroupMaySummariseOthers: a derived property scoped to a pod would be a
// subject summarising itself; a group is the one subject made of others.
func TestOnlyAGroupMaySummariseOthers(t *testing.T) {
	m := groupMap(t)
	err := m.DeclareGroup("pod:pg", []string{"pod:redis"}, nil)
	if err == nil || !strings.Contains(err.Error(), "group:") {
		t.Fatalf("err %v; want a group id refused unless it is group:<name>", err)
	}
	err = m.DeclareProperty(Property{ID: "x@pod:pg", Kind: Derived, Subject: "pod:pg", Members: []string{"cpu_utilization@pod:redis"}})
	if err == nil {
		t.Fatal("a derived property scoped to a pod was accepted")
	}
}

// TestWithoutAGroupTakesEveryMemberAway: "what would the node do without the storage
// group" floors every member's properties, the way without=<subject> floors one.
func TestWithoutAGroupTakesEveryMemberAway(t *testing.T) {
	m := groupMap(t)
	if err := m.DeclareGroup("group:storage", []string{"pod:pg", "pod:redis"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"cpu_utilization@pod:pg", "cpu_utilization@pod:redis"} {
		r := Relationship{From: from, To: "cpu_contention_ratio", Sign: 1, Label: "drives"}
		if err := m.DeclareRelationship(r); err != nil {
			t.Fatal(err)
		}
		if err := m.AssertRelationshipStrength(RelationshipID(r.From, r.To, r.Label), 0.5, "test", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	res := m.Estimate(EstimateRequest{Target: "cpu_contention_ratio", Without: []string{"group:storage"}})
	if res.Err != "" || res.Hypothetical == nil {
		t.Fatalf("result %+v", res)
	}
	// Both members to zero: 0.5*0.2 + 0.5*0.1 = 0.15 taken off.
	if !groupNearly(res.Hypothetical.Delta, -0.15) {
		t.Fatalf("delta %v; want both members floored, -0.15", res.Hypothetical.Delta)
	}
}

func groupNearly(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

// TestRedeclaringMembersRecordsThePreviousList: a membership change journals the list
// it replaced, so the change can be read without diffing two snapshots.
func TestRedeclaringMembersRecordsThePreviousList(t *testing.T) {
	m := groupMap(t)
	if err := m.DeclareGroup("group:storage", []string{"pod:pg", "pod:redis"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.DeclareGroup("group:storage", []string{"pod:pg"}, nil); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range m.Journal().Events(0, 0) {
		before, ok := e.Detail["members_before"].([]string)
		if e.Kind != EventPropertyRedeclared || !ok {
			continue
		}
		found = true
		after, _ := e.Detail["members"].([]string)
		if len(before) != 2 || len(after) != 1 || !slices.Contains(before, after[0]) {
			t.Fatalf("detail %v; want two members before and the first one after", e.Detail)
		}
	}
	if !found {
		t.Fatal("no property.redeclared event carries members_before")
	}
}
