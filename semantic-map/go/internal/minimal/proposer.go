package minimal

import (
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// DisabledProposer is the ProposerContract implementation for a node that should
// not mine for structure (-proposer=false): it satisfies the contract with no-ops
// and never emits a candidate. MICorrelationProposer is the one that does.
type DisabledProposer struct{}

func NewDisabledProposer() *DisabledProposer { return &DisabledProposer{} }

func (p *DisabledProposer) Observe(_, _ string, _, _ float64) error                   { return nil }
func (p *DisabledProposer) ObserveConstruct(_ string, _ float64) error                { return nil }
func (p *DisabledProposer) GetCandidates() ([]*types.CandidateEdge, error)            { return nil, nil }
func (p *DisabledProposer) Confirm(string) (*types.Proposition, error)                { return nil, nil }
func (p *DisabledProposer) Reject(candidateID string) error                           { return nil }
func (p *DisabledProposer) Defer(candidateID string) error                            { return nil }
func (p *DisabledProposer) GetHistory() ([]*types.CandidateEdge, error)               { return nil, nil }
func (p *DisabledProposer) ObserveProperty(_, _ string, _ float64, _ time.Time) error { return nil }
func (p *DisabledProposer) Forget(_ string) error                                     { return nil }
