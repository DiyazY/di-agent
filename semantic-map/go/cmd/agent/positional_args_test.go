package main

import (
	"strings"
	"testing"
)

// TestCheckNoPositionalArgs pins the guard that catches the `-flag false`
// mistake. Go's flag package stops parsing at the first non-flag argument, so
// a space-separated boolean value becomes a positional arg and every flag after
// it is discarded without warning — the daemon starts and serves normally on
// defaults. See checkNoPositionalArgs for the incident this guards against.
func TestCheckNoPositionalArgs(t *testing.T) {
	if err := checkNoPositionalArgs(0, ""); err != nil {
		t.Errorf("no positional args should be accepted, got %v", err)
	}

	err := checkNoPositionalArgs(1, "false")
	if err == nil {
		t.Fatal(`positional arg "false" should be rejected`)
	}
	// The message has to name the actual mistake, not just complain about an
	// unexpected argument — the whole point is that the cause is non-obvious.
	for _, want := range []string{"false", "-flag=false", "silently ignored"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q missing %q", err.Error(), want)
		}
	}
}
