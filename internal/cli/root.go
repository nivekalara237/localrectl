package cli

import (
	"fmt"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "lrctl",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		// The Printer is stored even when the configuration is refused, so that main prints the
		// error in the format asked for.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			var notes logging.Notes
			cfg, err := config.LoadConfig(cmd.Flags(), &notes)

			p := logging.NewPrinter(cmd.OutOrStdout(), cmd.ErrOrStderr(), cfg.OutputOptions())
			ctx := logging.With(cmd.Context(), p)
			if err == nil {
				ctx = config.With(ctx, cfg)
			}
			cmd.SetContext(ctx)
			notes.Flush(p.Logger())
			return err
		},
	}
	// Only a flag error points to --help: the whole usage would hide the error, and break -o json.
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return config.Usage(fmt.Errorf("%w (see '%s --help')", err, c.CommandPath()))
	})

	root.AddCommand(
		newVersionCommand(),
		newConfigCommand(),
		newPingCommand(),
		newValidateProductCommand(),
	)

	config.AddFlags(root.PersistentFlags())
	logging.AddArgs(root.PersistentFlags())
	logging.AddLogArgs(root.PersistentFlags())

	return root
}

// ExitCode is the exit code of the process for err, the error of cmd run by ExecuteC: 0 without
// an error, 2 for a usage error, else the exit-code setting. A configuration that failed to
// load has no setting: 1.
func ExitCode(cmd *cobra.Command, err error) int {
	if err == nil {
		return 0
	}
	if config.IsUsageError(err) {
		return 2
	}
	if cmd != nil {
		if cfg := config.From(cmd.Context()); cfg != nil {
			return cfg.ExitCode
		}
	}
	return 1
}
