package cli

import "github.com/spf13/cobra"

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:          "lrctl",
		SilenceUsage: false,
		RunE: func(cmd *cobra.Command, args []string) error {

			return nil
		},
	}

	root.AddCommand(NewVersionCommand())
	root.AddCommand(NewConfigCommand())

	return root
}
