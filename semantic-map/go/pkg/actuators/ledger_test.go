package actuators

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLedgerReloads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "ledger.json")
	l, err := OpenLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	for _, e := range []Entry{
		{Action: "a", Subject: "y", Applied: 1, Expires: now.Add(time.Minute), NodeID: "n"},
		{Action: "a", Subject: "x", Applied: 2, Expires: now, NodeID: "n", Original: 4, OriginalSet: true, DecisionID: "d"},
	} {
		if err := l.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	l2, err := OpenLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.All(), l2.All()) || len(l2.All()) != 2 || l2.All()[0].Subject != "x" {
		t.Fatalf("%+v vs %+v", l.All(), l2.All())
	}
}

func TestLedgerAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")
	l, err := OpenLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Put(Entry{Action: "a", Subject: "s", NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "ledger.json" {
		t.Fatalf("dir holds %v", ents)
	}
	b, _ := os.ReadFile(p)
	var f struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 || len(f.Entries) != 1 {
		t.Fatalf("%v %s", err, b)
	}
}

func TestLedgerCorruptIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	os.WriteFile(p, []byte("{nope"), 0o644)
	if _, err := OpenLedger(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("err = %v", err)
	}
}

func TestForFiltersByAction(t *testing.T) {
	l := MemoryLedger()
	l.Put(Entry{Action: "a", Subject: "s1", NodeID: "n"})
	l.Put(Entry{Action: "b", Subject: "s1", NodeID: "n"})
	l.Put(Entry{Action: "a", Subject: "s2", NodeID: "n"})
	if got := l.For("a"); len(got) != 2 || got[0].Subject != "s1" || got[1].Subject != "s2" {
		t.Fatalf("For(a) = %+v", got)
	}
	if got := l.For("c"); len(got) != 0 {
		t.Fatalf("For(c) = %+v", got)
	}
}

// TestLedgerRefusesAnInvalidEntry: an entry that breaks the ledger's invariants
// is a caller's bug and is refused, naming what is wrong.
func TestLedgerRefusesAnInvalidEntry(t *testing.T) {
	ok := Entry{Action: "a", Subject: "s", NodeID: "n", Applied: 2}
	cases := map[string]func(*Entry){
		"no action":                    func(e *Entry) { e.Action = "" },
		"no subject":                   func(e *Entry) { e.Subject = "" },
		"no node":                      func(e *Entry) { e.NodeID = "" },
		"applied NaN":                  func(e *Entry) { e.Applied = math.NaN() },
		"original Inf":                 func(e *Entry) { e.Original, e.OriginalSet = math.Inf(1), true },
		"prev applied NaN":             func(e *Entry) { e.Pending, e.PrevApplied, e.PrevSet = true, math.NaN(), true },
		"committed with a prev":        func(e *Entry) { e.PrevApplied, e.PrevSet = 1, true },
		"committed with a prev value":  func(e *Entry) { e.PrevApplied = 1 },
		"pending prev value, not set":  func(e *Entry) { e.Pending, e.PrevApplied = true, 1 },
		"original value with none set": func(e *Entry) { e.Original = 3 },
	}
	for name, mutate := range cases {
		e := ok
		mutate(&e)
		if err := MemoryLedger().Put(e); err == nil {
			t.Errorf("%s: Put(%+v) accepted", name, e)
		}
	}
	if err := MemoryLedger().Put(ok); err != nil {
		t.Fatalf("valid entry refused: %v", err)
	}
	pending := ok
	pending.Pending, pending.PrevApplied, pending.PrevSet = true, 1, true
	if err := MemoryLedger().Put(pending); err != nil {
		t.Fatalf("valid pending entry refused: %v", err)
	}
}

// TestLedgerPutRefusesAChangedOriginal: the Original of an entry is what the
// setting was before the agent's first change; a Put that would change it is a
// buggy caller, and is an error rather than silently ignored.
func TestLedgerPutRefusesAChangedOriginal(t *testing.T) {
	l := MemoryLedger()
	first := Entry{Action: "a", Subject: "s", NodeID: "n", Original: 1, OriginalSet: true, Applied: 2}
	if err := l.Put(first); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		{Action: "a", Subject: "s", NodeID: "n", Original: 5, OriginalSet: true, Applied: 3},
		{Action: "a", Subject: "s", NodeID: "n", Applied: 3},
	} {
		if err := l.Put(e); err == nil || !strings.Contains(err.Error(), "original") {
			t.Errorf("Put(%+v) err %v; want a refusal naming the original", e, err)
		}
	}
	if got := l.All(); len(got) != 1 || got[0] != first {
		t.Fatalf("ledger %+v; want the first entry unchanged", got)
	}
}

// TestOpenLedgerRefusesAnInvalidEntryOnDisk: a bad entry on disk is an error
// naming the file and the entry, not loaded into the ledger.
func TestOpenLedgerRefusesAnInvalidEntryOnDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	b, _ := json.Marshal(ledgerFile{Version: 1, Entries: []Entry{{Action: "memory.high", Subject: "pod:x", Applied: 1}}})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := OpenLedger(p)
	if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "pod:x") {
		t.Fatalf("err %v; want a refusal naming %s and the entry", err, p)
	}
}

// TestTheLedgerExportsNoTestHook: a hook that makes saves fail is for tests and
// must not be reachable by an importer.
func TestTheLedgerExportsNoTestHook(t *testing.T) {
	if _, ok := reflect.TypeOf(&Ledger{}).MethodByName("FailSaves"); ok {
		t.Fatal("Ledger exports FailSaves")
	}
}
