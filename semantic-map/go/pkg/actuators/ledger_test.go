package actuators

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLedgerPutKeepsFirstOriginal(t *testing.T) {
	l := MemoryLedger()
	if err := l.Put(Entry{Action: "a", Subject: "s", Original: 1, OriginalSet: true, Applied: 2}); err != nil {
		t.Fatal(err)
	}
	if err := l.Put(Entry{Action: "a", Subject: "s", Original: 5, OriginalSet: false, Applied: 3}); err != nil {
		t.Fatal(err)
	}
	all := l.All()
	if len(all) != 1 || all[0].Original != 1 || !all[0].OriginalSet || all[0].Applied != 3 {
		t.Fatalf("got %+v", all)
	}
}

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
	if err := l.Put(Entry{Action: "a", Subject: "s"}); err != nil {
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
