package agentmain

import (
	"slices"
	"testing"

	"github.com/DiyazY/di-agent/pkg/collectors"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

func TestSyncSourcesJournalsOnlyTransitions(t *testing.T) {
	state := statemap.New(statemap.Config{AdmitUnknown: true}, statemap.NewJournal(0))
	sync := func(st string) {
		syncSources(state, []collectors.MemberStatus{{Name: "netdata", State: st, LastError: "dial"}})
	}
	sync("starting") // never journals
	sync("backoff")
	sync("backoff")
	sync("ok")
	sync("ok")
	sync("starting")
	sync("stuck")

	var got []statemap.EventKind
	for _, e := range state.Journal().Events(0, 0) {
		if e.Kind == statemap.EventCollectorDown || e.Kind == statemap.EventCollectorUp {
			got = append(got, e.Kind)
		}
	}
	want := []statemap.EventKind{statemap.EventCollectorDown, statemap.EventCollectorUp, statemap.EventCollectorDown}
	if !slices.Equal(got, want) {
		t.Errorf("events %v; want %v", got, want)
	}
}
