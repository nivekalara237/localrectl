package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/nivekalara237/localrectl/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const configHelp = `
Configuration file (--config, YAML): the same names as the flags.
Use absolute paths: relative ones are resolved from the working directory.

  server-url: https://registry.example.com
  server-token: 98797......52245
  ca-file: /etc/local-registry/ca-root.cert
`

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q for %q", args[0], cmd.CommandPath())
	}
	return nil
}

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print the effective configuration and where each value comes from",
		Long: `Resolve the configuration exactly as 'serve' would (flags, environment, configuration
file, defaults), validate it, and print each setting with its source. The upload token is
never printed: only whether it is set and its token_id, as written in the audit log.
` + configHelp,
		Example: `  lrctl config --config ~/.lr/config.yaml
  LOCAL_REGISTRY_CA_FILE=./root.cert local-registry config`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			/*err := config.LoadConfig(cmd.PersistentFlags(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}*/
			fmt.Println(",,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,,")
			printConfig(cmd.OutOrStdout(), config.Vconfig, cmd.Flags(), config.AppConfig)
			return nil
		},
	}

	return cmd
}

func configPath(v *viper.Viper) string {
	p, _ := config.ExpandPath(v.GetString("config"))
	return p
}

func printConfig(w io.Writer, v *viper.Viper, fs *pflag.FlagSet, cfg config.AppUntypedConfig) {

	rows := [][2]string{
		{"config", configPath(v)},
		{"server.url", cfg.Server.BaseUrl},
		{"server.upload-token", cfg.Server.UploadToken[:7] + "*****"},
		{"server.ca-file", cfg.Server.CaFile},
		{"output", fmt.Sprint(cfg.Output)},
		{"retries", fmt.Sprint(cfg.Retries)},
		{"timeout", fmt.Sprint(cfg.Timeout)},
		{"no-color", fmt.Sprint(cfg.NoColor)},
		{"verbose", fmt.Sprint(cfg.Verbose)},
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SETTING\tVALUE\tSOURCE")
	for _, r := range rows {
		val := r[1]
		if val == "" {
			val = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r[0], val, config.Source(fs, r[0]))
	}
	tw.Flush()
}
