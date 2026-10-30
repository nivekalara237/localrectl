package cli

import (
	"fmt"
	"io"

	"github.com/nivekalara237/localrectl/internal/build"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version",
		Aliases: []string{
			"v",
		},
		Short: "Get Version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := build.GetBuildInfo()
			return logging.From(cmd.Context()).Result(info, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, info.ToString())
				return err
			})
		},
	}
}
