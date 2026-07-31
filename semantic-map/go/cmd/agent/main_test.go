package main

import (
	"os"
	"strings"
	"testing"
)

// TestFlagsReachConfig guards the class of defect this project has now hit three
// times: a flag is declared, parsed, logged, and never passed to the thing it
// configures. Each instance produced a daemon that ran normally while ignoring the
// operator — -tuner wired DisabledTuner, -proposer's space-separated value
// discarded every flag after it, and -machine-class seeded class-averaged priors
// while logging that it had applied class-specific ones.
//
// Comparing the flag set against the Config fields it must populate catches the
// next one without waiting for a measurement to look wrong.
func TestFlagsReachConfig(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// Each entry is a flag and the Config field its value must reach. Extend this
	// when adding a flag that configures the profile.
	wiring := map[string]string{
		"machine-class": "MachineClass:",
		"ingest-scope":  "AcceptForeignSamples:",
		"relational":    "Relational:",
		"pair-window":   "PairWindowSeconds:",
		"tuner":         "UseRuleBasedTuner:",
		"proposer":      "UseProposer:",
		"kd":            "KD:",
		"priors":        "PriorWeightsPath:",
		"domain":        "DomainSpec:",
	}
	for flagName, field := range wiring {
		if !strings.Contains(body, `"`+flagName+`"`) {
			t.Errorf("flag %q is no longer declared; update this test or restore it", flagName)
			continue
		}
		if !strings.Contains(body, field) {
			t.Errorf("flag %q is declared but no Config literal sets %s — the daemon will "+
				"accept the flag and ignore it", flagName, field)
		}
	}
}
