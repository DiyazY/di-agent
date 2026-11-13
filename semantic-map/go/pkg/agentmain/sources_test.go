package agentmain

import (
	"testing"

	"github.com/DiyazY/di-agent/pkg/collectors"
	"github.com/DiyazY/di-agent/pkg/statemap"
)

// An unhealthy member is down like one in backoff: collector.down carries its
// reason, and a healthy tick brings collector.up.
func TestSyncSourcesTakesUnhealthyAsDown(t *testing.T) {
	m := statemap.New(statemap.Config{Owner: "n1"}, nil)
	syncSources(m, []collectors.MemberStatus{{Name: "netdata", State: "unhealthy", LastError: "connection refused"}})
	syncSources(m, []collectors.MemberStatus{{Name: "netdata", State: "ok"}})
	var kinds []string
	for _, e := range m.Journal().Events(0, 0) {
		switch e.Kind {
		case statemap.EventCollectorDown:
			if e.Detail["reason"] != "connection refused" {
				t.Errorf("collector.down detail = %v, want the reason", e.Detail)
			}
			kinds = append(kinds, string(e.Kind))
		case statemap.EventCollectorUp:
			kinds = append(kinds, string(e.Kind))
		}
	}
	if len(kinds) != 2 || kinds[0] != "collector.down" || kinds[1] != "collector.up" {
		t.Fatalf("events = %v, want collector.down then collector.up", kinds)
	}
}
