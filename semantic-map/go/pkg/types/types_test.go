package types

import "testing"

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
