package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/DiyazY/di-agent/cmd/mapctl/client"
	"github.com/DiyazY/di-agent/cmd/mapctl/render"
)

// newCandidatesCmd returns `mapctl candidates [list|confirm|reject|defer]`.
// `list` is the default action when no subcommand is given.
func newCandidatesCmd(deps *Deps) *cobra.Command {
	parent := &cobra.Command{
		Use:   "candidates",
		Short: "Inspect or review pending candidate edges",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default behavior: list. Subcommands handle their own RunE.
			return runCandidatesList(cmd, deps)
		},
	}

	parent.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List pending candidate edges (GET /candidates)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCandidatesList(cmd, deps)
		},
	})

	parent.AddCommand(&cobra.Command{
		Use:   "confirm <candidate-id>",
		Short: "Confirm a candidate edge (POST /candidates/{id}/confirm)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := deps.Client().ConfirmCandidate(deps.Ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "confirmed %s\n", args[0])
			return nil
		},
	})

	parent.AddCommand(&cobra.Command{
		Use:   "reject <candidate-id>",
		Short: "Reject a candidate edge (POST /candidates/{id}/reject)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := deps.Client().RejectCandidate(deps.Ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rejected %s\n", args[0])
			return nil
		},
	})

	parent.AddCommand(&cobra.Command{
		Use:   "defer <candidate-id>",
		Short: "Defer a candidate edge (POST /candidates/{id}/defer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := deps.Client().DeferCandidate(deps.Ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deferred %s\n", args[0])
			return nil
		},
	})

	return parent
}

// runCandidatesList is the shared list implementation used by the bare
// `candidates` command and `candidates list`.
func runCandidatesList(cmd *cobra.Command, deps *Deps) error {
	candidates, err := deps.Client().Candidates(deps.Ctx)
	if err != nil {
		return err
	}
	if deps.JSON {
		return render.JSON(cmd.OutOrStdout(), candidates)
	}
	renderCandidatesTable(cmd.OutOrStdout(), candidates)
	return nil
}

// renderCandidatesTable formats candidate edges for terminal output.
func renderCandidatesTable(w io.Writer, candidates []client.CandidateEdge) {
	if len(candidates) == 0 {
		fmt.Fprintln(w, "(no pending candidates)")
		return
	}
	rows := make([][]string, 0, len(candidates))
	for _, c := range candidates {
		rows = append(rows, []string{
			c.CandidateID, c.FromID, c.ToID,
			directionLabel(c.Direction),
			fmt.Sprintf("%.3f", c.MIScore),
			fmt.Sprintf("%.3g", c.PValue),
			fmt.Sprintf("%d", c.NObservations),
			fmt.Sprintf("%d", c.DeploymentsSeen),
			statusLabel(c.Status, c.Reason),
		})
	}
	render.Table(w, []string{"ID", "From", "To", "Dir", "MI", "p-value", "N", "Deploys", "Status"}, rows)
}

// directionLabel maps the int Direction to its wire-readable form, matching
// the server's "+"/"-" rendering. Kept in this file because no other command
// renders a raw int direction.
func directionLabel(d int) string {
	if d == 1 {
		return "-"
	}
	return "+"
}

// statusLabel renders the named status the agent sends, with who deferred a deferred
// candidate ("deferred (cap)" vs "deferred (operator)").
func statusLabel(status, reason string) string {
	if reason != "" {
		return status + " (" + reason + ")"
	}
	return status
}
