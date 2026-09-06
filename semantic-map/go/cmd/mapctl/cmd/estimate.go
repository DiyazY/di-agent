package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/DiyazY/di-agent/cmd/mapctl/render"
)

// newEstimateCmd returns `mapctl estimate <target> [--assume p=v]... [--without x]...`.
func newEstimateCmd(deps *Deps) *cobra.Command {
	var assumes, withouts []string
	c := &cobra.Command{
		Use:   "estimate <target>",
		Short: "Ask the map about a property, optionally under hypotheses (GET /state/estimate)",
		Long: "Answers from the map's relationships into <target>. --assume substitutes a source\n" +
			"property's value; --without takes a subject's properties to their floor. The\n" +
			"answer carries a decision id whose inputs and assumptions can be replayed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			assume := map[string]float64{}
			for _, a := range assumes {
				k, v, ok := strings.Cut(a, "=")
				if !ok || k == "" {
					return fmt.Errorf("--assume must be <property>=<value>: %q", a)
				}
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return fmt.Errorf("--assume value must be a number: %q", a)
				}
				assume[k] = f
			}
			res, err := deps.Client().Estimate(deps.Ctx, args[0], assume, withouts)
			if err != nil {
				return err
			}
			if deps.JSON {
				return render.JSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s: level %.4f (confidence %.3f, %s)\n", res.Answer.Target, res.Answer.Level, res.Answer.Confidence, res.Answer.Status)
			fmt.Fprintf(out, "sensitivity %+.4f per normalised unit; contributions %+.4f at current values\n", res.Answer.Sensitivity, res.Answer.Contributions)
			if res.Hypothetical != nil {
				fmt.Fprintf(out, "under the assumptions: delta %+.4f, projected level %.4f\n", res.Hypothetical.Delta, res.Hypothetical.ProjectedLevel)
			}
			rows := make([][]string, 0, len(res.Influences))
			for _, in := range res.Influences {
				hyp := ""
				if in.HypotheticalSourceValue != nil {
					hyp = strconv.FormatFloat(*in.HypotheticalSourceValue, 'g', -1, 64)
				}
				strength := "—" // no strength yet is not a strength of zero
				if in.Known {
					strength = strconv.FormatFloat(in.Strength, 'g', 3, 64)
				}
				rows = append(rows, []string{in.Source, strconv.FormatFloat(in.SourceValue, 'g', 4, 64),
					fmt.Sprintf("%+d", in.Sign), strength, in.Basis, hyp})
			}
			render.Table(out, []string{"SOURCE", "VALUE", "SIGN", "STRENGTH", "BASIS", "ASSUMED"}, rows)
			for _, cv := range res.Caveats {
				fmt.Fprintf(out, "caveat: %s\n", cv)
			}
			fmt.Fprintf(out, "decision %s (revision %d)\n", res.DecisionID, res.Revision)
			return nil
		},
	}
	c.Flags().StringArrayVar(&assumes, "assume", nil, "hypothetical source value, <property>=<value> (repeatable)")
	c.Flags().StringArrayVar(&withouts, "without", nil, "subject or property taken to its floor (repeatable)")
	return c
}
