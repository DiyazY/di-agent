package agentmain

import (
	"log"

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

// resolveChain is Journal.Chain with the log behind it. A held decision's chain
// comes from the journal, each link it marks evicted filled from the log when the
// log has it (its own ancestors are not walked further). One only the log holds is walked depth-first in cite
// order, each id once, every hop through resolveDecision; an id neither holds is
// marked evicted. CitedBy is nil: the log has no reverse index.
func resolveChain(j *statemap.Journal, id string) (statemap.Chain, string, bool) {
	if c, ok := j.Chain(id); ok {
		for i, l := range c.Cites {
			if !l.Evicted {
				continue
			}
			if d, _, held := resolveDecision(j, l.ID); held {
				c.Cites[i] = statemap.ChainLink{ID: l.ID, Decision: d}
			}
		}
		return c, "journal", true
	}
	d, from, ok := resolveDecision(j, id)
	if !ok {
		return statemap.Chain{}, "", false
	}
	c := statemap.Chain{Decision: d, Cites: []statemap.ChainLink{}}
	seen := map[string]bool{id: true}
	stack := slicesReversed(d.Cites)
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
		stack = append(stack, slicesReversed(a.Cites)...)
	}
	return c, from, true
}

func slicesReversed(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}
