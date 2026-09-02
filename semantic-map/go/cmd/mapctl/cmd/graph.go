package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/DiyazY/di-agent/cmd/mapctl/client"
	"github.com/DiyazY/di-agent/cmd/mapctl/render"
)

// newGraphCmd returns `mapctl graph`, which prints the full graph snapshot
// as three small tables (Constructs, Propositions, Edges) or as JSON.
func newGraphCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "graph",
		Short: "Show the full graph snapshot (constructs, propositions, edges)",
		RunE: func(cmd *cobra.Command, args []string) error {
			snap, err := deps.Client().Graph(deps.Ctx)
			if err != nil {
				return err
			}
			if deps.JSON {
				return render.JSON(cmd.OutOrStdout(), snap)
			}
			renderGraphTables(cmd.OutOrStdout(), snap)
			return nil
		},
	}
}

// renderGraphTables prints three labeled tables, one per top-level slice of
// GraphSnapshot. Keeping it in graph.go (not render/) preserves the
// per-command-formatter rule.
func renderGraphTables(w io.Writer, snap *client.GraphSnapshot) {
	fmt.Fprintf(w, "Constructs (%d)\n", len(snap.Constructs))
	cRows := make([][]string, 0, len(snap.Constructs))
	for _, c := range snap.Constructs {
		cRows = append(cRows, []string{c.ConstructID, c.Name, c.Description})
	}
	render.Table(w, []string{"ID", "Name", "Description"}, cRows)

	fmt.Fprintf(w, "\nPropositions (%d)\n", len(snap.Propositions))
	pRows := make([][]string, 0, len(snap.Propositions))
	for _, p := range snap.Propositions {
		dep := ""
		if p.Deprecated {
			dep = "yes"
		}
		pRows = append(pRows, []string{
			p.PropositionID, p.FromConstruct, p.ToConstruct, p.Direction,
			fmt.Sprintf("%.2f", p.PriorStrength), dep,
		})
	}
	render.Table(w, []string{"ID", "From", "To", "Dir", "Strength", "Deprecated"}, pRows)

	fmt.Fprintf(w, "\nEdges (%d)\n", len(snap.Edges))
	eRows := make([][]string, 0, len(snap.Edges))
	for _, e := range snap.Edges {
		eRows = append(eRows, []string{
			e.PropositionID, e.FromID, e.ToID, e.Direction,
			edgeBasis(e.Basis),
			ptrToString(e.Effective, "%.3f"),
			fmt.Sprintf("%.3f", e.EMAWeight),
			fmt.Sprintf("%.2f", e.Confidence),
			fmt.Sprintf("%d", e.NObservations),
		})
	}
	render.Table(w, []string{"PropID", "From", "To", "Dir", "Basis", "Effective", "Recent", "Conf", "N"}, eRows)
}
