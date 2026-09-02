package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/DiyazY/di-agent/cmd/mapctl/client"
	"github.com/DiyazY/di-agent/cmd/mapctl/render"
)

// newEdgesCmd returns `mapctl edges [--from --to]`. Without filters it lists
// every edge; either or both filters narrow the set. The same conflict-pair
// (RC→PS, P2 + P3) ships as a smoke test in the verification block.
func newEdgesCmd(deps *Deps) *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:   "edges",
		Short: "List edges, optionally filtered by --from / --to",
		RunE: func(cmd *cobra.Command, args []string) error {
			edges, err := deps.Client().Edges(deps.Ctx, from, to)
			if err != nil {
				return err
			}
			if deps.JSON {
				return render.JSON(cmd.OutOrStdout(), edges)
			}
			renderEdgesTable(cmd.OutOrStdout(), edges)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source construct ID")
	cmd.Flags().StringVar(&to, "to", "", "target construct ID")
	return cmd
}

// renderEdgesTable writes a per-edge table. Mu/Sigma render as a literal
// "-" when nil to keep the column width predictable.
func renderEdgesTable(w io.Writer, edges []client.EdgeDTO) {
	rows := make([][]string, 0, len(edges))
	for _, e := range edges {
		rows = append(rows, []string{
			e.PropositionID, e.FromID, e.ToID, e.Direction,
			edgeBasis(e.Basis),
			ptrToString(e.Effective, "%.3f"),
			ptrToString(e.Established, "%.3f"),
			ptrToString(e.Assertion, "%.3f"),
			fmt.Sprintf("%.3f", e.EMAWeight),
			fmt.Sprintf("%.2f", e.Confidence),
			fmt.Sprintf("%d", e.NObservations),
			ptrToString(e.Mu, "%.3f"),
			ptrToString(e.Sigma, "%.3f"),
		})
	}
	render.Table(w, []string{"PropID", "From", "To", "Dir", "Basis", "Effective", "Establ", "Assert", "Recent", "Conf", "N", "Mu", "Sigma"}, rows)
}

// edgeBasis renders an empty basis as "unknown" rather than blank. A relationship
// with no evidence has a definite state and the table should say so; a blank cell
// reads as a rendering gap.
func edgeBasis(b string) string {
	if b == "" {
		return "unknown"
	}
	return b
}

// ptrToString formats a *float64 with the given verb, or "-" if nil.
func ptrToString(p *float64, format string) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf(format, *p)
}
