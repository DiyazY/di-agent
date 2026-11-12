package agentmain

import (
	"encoding/json"

	"github.com/DiyazY/di-agent/pkg/admission"
	"github.com/DiyazY/di-agent/pkg/peers"
	"github.com/DiyazY/di-agent/pkg/relief"
)

// loopsBlob is the loops' learned state as the snapshot carries it. NodeID
// says whose it is: what relief learned about this node's workloads, and the
// trust this node extended, mean nothing on another node.
type loopsBlob struct {
	NodeID    string                      `json:"node_id"`
	Relief    map[string]relief.Memory    `json:"relief"` // by action
	Admission admission.Memory            `json:"admission"`
	Peers     map[string]peers.TrustState `json:"peers"` // by peer id
}

// captureLoops is the snapshot's capture of the loops' learned state.
func captureLoops(nodeID string, loops []*relief.Loop, tr *admission.Tracker, reg *peers.Registry) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) {
		b := loopsBlob{NodeID: nodeID, Relief: map[string]relief.Memory{}, Admission: tr.Memory(), Peers: reg.Trusts()}
		for _, l := range loops {
			b.Relief[l.Lever().Action] = l.Memory()
		}
		return json.Marshal(b)
	}
}

// restoreLoops hands the saved state back to the loops, before any of them
// starts. Nothing saved is a cold start; state saved by another node is ignored.
// A lever absent from the blob starts fresh, and a saved action with no loop
// here is dropped.
func restoreLoops(raw json.RawMessage, nodeID string, loops []*relief.Loop, tr *admission.Tracker, reg *peers.Registry, logf func(string, ...any)) error {
	if len(raw) == 0 {
		return nil
	}
	var b loopsBlob
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	if b.NodeID != nodeID {
		logf("loops: snapshot state belongs to %q, this node is %q: ignored", b.NodeID, nodeID)
		return nil
	}
	for _, l := range loops {
		if mem, ok := b.Relief[l.Lever().Action]; ok {
			l.Restore(mem)
		}
	}
	tr.Restore(b.Admission)
	reg.RestoreTrust(b.Peers)
	return nil
}
