package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/DiyazY/di-agent/cmd/mapctl/client"
	"github.com/DiyazY/di-agent/cmd/mapctl/render"
)

// newRecommendCmd returns `mapctl recommend`, which POSTs to /recommend. The
// flags model an OffloadContext; the server's response is printed as JSON or
// as a small key/value table.
func newRecommendCmd(deps *Deps) *cobra.Command {
	var (
		task      string
		source    string
		size      int64
		latencyMs float64
		energyJ   float64
		energySet bool
	)
	cmd := &cobra.Command{
		Use:   "recommend",
		Short: "Recommend a peer for offload (POST /recommend)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := client.OffloadContext{
				TaskType:        task,
				SourceNodeID:    source,
				DataSizeBytes:   size,
				LatencyBudgetMs: latencyMs,
			}
			if energySet {
				e := energyJ
				ctx.EnergyBudgetJoules = &e
			}
			rec, err := deps.Client().Recommend(deps.Ctx, ctx)
			if err != nil {
				return err
			}
			if deps.JSON {
				return render.JSON(cmd.OutOrStdout(), rec)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Peer:           %s\n", rec.PeerID)
			fmt.Fprintf(cmd.OutOrStdout(), "ExpectedSavings: %.3f\n", rec.ExpectedSavings)
			fmt.Fprintf(cmd.OutOrStdout(), "GraphPath:      %s\n", strings.Join(rec.GraphPathUsed, " -> "))
			fmt.Fprintf(cmd.OutOrStdout(), "Rationale:      %s\n", rec.Rationale)
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task type (e.g. pod-scheduling)")
	cmd.Flags().StringVar(&source, "source", "", "source node ID")
	cmd.Flags().Int64Var(&size, "size", 0, "data size in bytes")
	cmd.Flags().Float64Var(&latencyMs, "latency-ms", 0, "latency budget in milliseconds")
	cmd.Flags().Float64Var(&energyJ, "energy-j", 0, "optional energy budget in joules")
	cmd.PreRun = func(cmd *cobra.Command, args []string) {
		energySet = cmd.Flags().Changed("energy-j")
	}
	return cmd
}
