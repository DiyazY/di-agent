package agentmain

import (
	"log"
	"slices"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

// resolveDecision finds a decision in the bounded journal, then in the journal
// log. from is "journal" or "log". A log that cannot be read is logged and reads
// as a miss: the caller answers as though neither held the decision.
func resolveDecision(j *statemap.Journal, id string) (d *statemap.Decision, from string, ok bool) {
	if d, ok := j.Decision(id); ok {
		return d, "journal", true
	}
	d, ok, err := j.DecisionFromLog(id)
	if err != nil {
		log.Printf("journal log: reading %s: %v", id, err)
		return nil, "", false
	}
	if !ok {
		return nil, "", false
	}
	return d, "log", true
}

// resolveChain is Journal.Chain with the log behind it: the chain is walked hop
// by hop through resolveDecision (memory first, then the log), depth-first in cite
// order, each id once; an id neither holds is marked evicted. CitedBy comes from
// the journal when the root is held in memory, and is nil otherwise: the log has
// no reverse index.
func resolveChain(j *statemap.Journal, id string) (statemap.Chain, string, bool) {
	d, from, ok := resolveDecision(j, id)
	if !ok {
		return statemap.Chain{}, "", false
	}
	c := statemap.Chain{Decision: d, Cites: []statemap.ChainLink{}}
	if held, ok := j.Chain(id); ok {
		c.CitedBy = held.CitedBy
	}
	seen := map[string]bool{id: true}
	stack := reversed(d.Cites) // an explicit stack: a chain can be long
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		a, _, held := resolveDecision(j, next)
		if !held {
			c.Cites = append(c.Cites, statemap.ChainLink{ID: next, Evicted: true})
			continue
		}
		c.Cites = append(c.Cites, statemap.ChainLink{ID: next, Decision: a})
		stack = append(stack, reversed(a.Cites)...)
	}
	return c, from, true
}

func reversed(ids []string) []string {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}
