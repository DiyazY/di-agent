// Package cmd contains the cobra subcommands that comprise the mapctl CLI.
//
// Design:
//   - One subcommand per file, each exporting newXxxCmd(deps Deps) *cobra.Command.
//   - root.go wires every subcommand via rootCmd.AddCommand(newXxxCmd(deps))
//     — no init() registration, no package-level globals beyond the
//     persistent flags.
//   - Persistent flags (--addr, --json, --no-color) live on rootCmd and are
//     read through the Deps struct, which is constructed per-Execute.
package cmd

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/DiyazY/di-agent/cmd/mapctl/client"
)

// ClientVersion is the build-time semver of the CLI itself. The server's
// version is fetched at runtime via GET /version.
const ClientVersion = "0.1.0"

// Deps bundles everything a subcommand needs that depends on the persistent
// flags. Construct via newDeps() inside PersistentPreRunE so the flags have
// been parsed by the time deps are wired.
type Deps struct {
	// Addr is the resolved --addr value, e.g. "http://localhost:8080".
	Addr string
	// JSON is the global --json flag.
	JSON bool
	// NoColor is the global --no-color flag.
	NoColor bool
	// Client returns a fresh *client.Client targeted at Addr.
	Client func() *client.Client
	// Ctx is the root context honoring Ctrl-C via signal.NotifyContext.
	// Wired by Execute().
	Ctx context.Context
}

// rootFlags holds the parsed values of the persistent flags. It is the
// single source of truth that PersistentPreRunE reads into Deps.
type rootFlags struct {
	addr    string
	json    bool
	noColor bool
}

// Execute builds the command tree, wires deps, and runs cobra. Returns the
// first error so main can set exit code.
func Execute() error {
	return NewRootCmd().Execute()
}

// NewRootCmd builds a fresh root command and its full subcommand tree. It is
// exported so tests can introspect the help output and flag set without
// running anything.
func NewRootCmd() *cobra.Command {
	flags := &rootFlags{}
	deps := &Deps{}

	rootCmd := &cobra.Command{
		Use:   "mapctl",
		Short: "Control surface for the di-agent semantic-map daemon",
		Long: "mapctl is a thin HTTP client for the di-agent semantic-map\n" +
			"daemon. It exposes read endpoints (graph, edges, history,\n" +
			"candidates, ...) and mutation endpoints (strength, deprecate,\n" +
			"construct, proposition, reset, candidate review) as cobra\n" +
			"subcommands. Global --json switches every subcommand to JSON.",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	rootCmd.PersistentFlags().StringVar(&flags.addr, "addr", defaultAddr(), "agent HTTP address")
	rootCmd.PersistentFlags().BoolVar(&flags.json, "json", false, "emit JSON instead of tables")
	rootCmd.PersistentFlags().BoolVar(&flags.noColor, "no-color", false, "disable ANSI color/clear codes")

	// PersistentPreRunE runs after flag parsing for every subcommand,
	// including subcommands of subcommands. It hydrates the deps from
	// the parsed flag values.
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		deps.Addr = flags.addr
		deps.JSON = flags.json
		deps.NoColor = flags.noColor
		deps.Client = func() *client.Client { return client.New(flags.addr) }
		if deps.Ctx == nil {
			deps.Ctx = cmd.Context()
		}
		return nil
	}

	rootCmd.AddCommand(
		newGraphCmd(deps),
		newEdgesCmd(deps),
		newHistoryCmd(deps),
		newStrengthCmd(deps),
		newDeprecateCmd(deps),
		newConstructCmd(deps),
		newPropositionCmd(deps),
		newResetCmd(deps),
		newCandidatesCmd(deps),
		newEstimateCmd(deps),
		newRecommendCmd(deps),
		newSimulateCmd(deps),
		newWatchCmd(deps),
		newDotCmd(deps),
		newHealthCmd(deps),
		newPeersCmd(deps),
		newVersionCmd(deps),
		newTuneCmd(deps),
		newCompletionCmd(),
	)

	return rootCmd
}

// defaultAddr returns the default agent address, honoring MAPCTL_ADDR if set.
func defaultAddr() string {
	if v := os.Getenv("MAPCTL_ADDR"); v != "" {
		return v
	}
	return "http://localhost:8080"
}
