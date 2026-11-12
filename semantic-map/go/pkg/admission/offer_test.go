package admission

import (
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		offer   Offer
		wantErr bool
	}{
		{"empty name", Offer{Demand: map[string]Demand{"cpu": {Value: 1}}}, true},
		{"unknown key", Offer{Name: "w", Demand: map[string]Demand{"gpu": {Value: 1}}}, true},
		{"negative", Offer{Name: "w", Demand: map[string]Demand{"cpu": {Value: -1}}}, true},
		{"nan", Offer{Name: "w", Demand: map[string]Demand{"memory": {Value: math.NaN()}}}, true},
		{"inf", Offer{Name: "w", Demand: map[string]Demand{"cpu": {Value: math.Inf(1)}}}, true},
		{"zero cpu", Offer{Name: "w", Demand: map[string]Demand{"cpu": {Value: 0}}}, false},
		{"no demand", Offer{Name: "w"}, false},
	}
	for _, c := range cases {
		err := c.offer.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func TestIDsDistinctUnderConcurrency(t *testing.T) {
	m := statemap.New(statemap.Config{}, nil)
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := m.NewDecisionID("admit")
			mu.Lock()
			seen[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 100 {
		t.Fatalf("got %d distinct ids, want 100", len(seen))
	}
	for id := range seen {
		if !strings.HasPrefix(id, "admit-1.") {
			t.Errorf("id %q lacks prefix admit-1.", id)
		}
	}
}

func TestAsked(t *testing.T) {
	o := Offer{Chain: []Hop{
		{Node: "a", URL: "http://a:1"},
		{Node: "b", URL: ""},
	}}
	if !o.Asked("a", "") {
		t.Error("node id a should match")
	}
	if !o.Asked("z", "http://a:1") {
		t.Error("url should match")
	}
	if o.Asked("z", "") {
		t.Error("empty URL must never match an empty hop URL")
	}
	if o.Asked("z", "http://z:1") {
		t.Error("unrelated node matched")
	}
	// An unreachable hop carries no node id; an empty id must not match it.
	o.Chain = append(o.Chain, Hop{Verdict: Unreachable, URL: "http://u:1"})
	if o.Asked("", "") || o.Asked("", "http://z:1") {
		t.Error("empty node id matched a hop without one")
	}
}
