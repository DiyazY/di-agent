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
