package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidSubject(t *testing.T) {
	good := []string{"", "pod:8f3c1234-aaaa-bbbb-cccc-1234567890ab", "unit:k0sworker.service", "disk:mmcblk0", "a:b:c"}
	bad := []string{"pod", "pod/x", ":x", "pod:", "pod:a b", "pod:x/y", "kind:iden tity"}
	for _, s := range good {
		if !ValidSubject(s) {
			t.Errorf("ValidSubject(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSubject(s) {
			t.Errorf("ValidSubject(%q) = true, want false", s)
		}
	}
}

// TestValidMetricType: a metric type is one path segment over [A-Za-z0-9._-]. '@'
// would let an unscoped sample named "cpu@pod:a" land on the scoped property's id.
func TestValidMetricType(t *testing.T) {
	for _, ok := range []string{"cpu_utilization", "io.wait", "queue-depth", "Requests2"} {
		if !ValidMetricType(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "cpu@pod:a", "a/b", "a b", "a->b", "a:b"} {
		if ValidMetricType(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestCandidateStatusIsNamedInJSON: as an integer, Pending serialised as 0, which a
// reader cannot tell from a missing field. The names go on the wire; numbers are
// still accepted on the way in for older clients.
func TestCandidateStatusIsNamedInJSON(t *testing.T) {
	b, err := json.Marshal(CandidateEdge{CandidateID: "a->b", Status: Pending})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"Status":"pending"`) {
		t.Errorf("%s lacks a named status", b)
	}
	var e CandidateEdge
	if err := json.Unmarshal([]byte(`{"CandidateID":"a->b","Status":"deferred"}`), &e); err != nil || e.Status != Deferred {
		t.Errorf("named status did not decode: %+v %v", e, err)
	}
	if err := json.Unmarshal([]byte(`{"CandidateID":"a->b","Status":3}`), &e); err != nil || e.Status != Deferred {
		t.Errorf("numeric status did not decode: %+v %v", e, err)
	}
	if Confirmed.String() != "confirmed" {
		t.Errorf("String() = %q", Confirmed.String())
	}
}
