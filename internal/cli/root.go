package cli

import (
	"fmt"

	"github.com/nivekalara237/localrectl/config"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:          "lrctl",
		SilenceUsage: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := config.LoadConfig(cmd.PersistentFlags(), cmd.ErrOrStderr())
			fmt.Println("!!!!!!!!!!!!!!!!!")
			return err
		},
	}

	root.AddCommand(newVersionCommand(), newConfigCommand())

	config.AddFlags(root.PersistentFlags())

	return root
}
