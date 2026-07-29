package explain

import "context"

// DisabledExplainer is the default implementation. It refuses every call with
// ErrNotEnabled so the daemon has a working Explainer field even when the
// operator has not configured a provider. This mirrors DisabledProposer's
// role in pkg/proposer — a benign no-op that keeps the interface satisfied.
type DisabledExplainer struct{}

// NewDisabled returns a fresh DisabledExplainer.
func NewDisabled() *DisabledExplainer { return &DisabledExplainer{} }

// Explain always returns (nil, ErrNotEnabled).
func (DisabledExplainer) Explain(ctx context.Context, req ExplainRequest) (*ExplainResponse, error) {
	return nil, ErrNotEnabled
}

// Close is a no-op.
func (DisabledExplainer) Close() error { return nil }
