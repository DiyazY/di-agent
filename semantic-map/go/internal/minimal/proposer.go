package minimal

import (
	"github.com/DiyazY/di-agent/pkg/types"
)

// DisabledProposer is the ProposerContract implementation for a node that should
// not mine for structure (-proposer=false): it satisfies the contract with no-ops
// and never emits a candidate. MICorrelationProposer is the one that does.
type DisabledProposer struct{}

func NewDisabledProposer() *DisabledProposer { return &DisabledProposer{} }

func (p *DisabledProposer) Observe(_, _ string, _, _ float64) error        { return nil }
func (p *DisabledProposer) ObserveConstruct(_ string, _ float64) error     { return nil }
func (p *DisabledProposer) GetCandidates() ([]*types.CandidateEdge, error) { return nil, nil }
func (p *DisabledProposer) Confirm(_, _ string, _ types.CandidateReview) (*types.Proposition, error) {
	return nil, nil
}
func (p *DisabledProposer) Reject(string, types.CandidateReview) error   { return nil }
func (p *DisabledProposer) Defer(string, types.CandidateReview) error    { return nil }
func (p *DisabledProposer) Annotate(string, types.CandidateReview) error { return nil }
func (p *DisabledProposer) GetHistory() ([]*types.CandidateEdge, error)  { return nil, nil }
func (p *DisabledProposer) ObserveProperty(types.PropertyReading) error  { return nil }
func (p *DisabledProposer) Forget(_ string) error                        { return nil }
func (p *DisabledProposer) Discovery() (*types.DiscoveryCounts, error)   { return nil, nil }
