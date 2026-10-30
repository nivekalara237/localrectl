package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

const configHelp = `
Configuration file (--config, YAML): the same names as the flags.
Use absolute paths: relative ones are resolved from the working directory.

  server:
    url: https://registry.example.com
    upload-token: "98797......52245"   # quoted: digits only would be read as a number
    ca-file: /etc/local-registry/ca-root.cert
`

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return config.Usage(fmt.Errorf("unexpected argument %q for %q", args[0], cmd.CommandPath()))
	}
	return nil
}
func withArgs(countArgs int) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != countArgs {
			return config.Usage(fmt.Errorf("the command %s waits exactly %d arg(s)", cmd.CommandPath(), countArgs))
		}
		return nil
	}
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
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("the configuration is not loaded: run this command from the root command")
			}
			view := newConfigView(cfg)
			return logging.From(cmd.Context()).Result(view, view.printText)
		},
	}

	return cmd
}

// maskToken keeps at most the first 7 characters, and none of a short token.
func maskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) < 14 {
		return "*****"
	}
	return t[:7] + "*****"
}

// setting is one line of 'lrctl config'. A nil Value is a setting with no value.
type setting struct {
	Name   string `json:"name"`
	Value  any    `json:"value"`
	Source string `json:"source"`
}

type configView struct {
	Settings []setting `json:"settings"`
}

func newConfigView(cfg *config.Config) configView {
	values := map[string]any{
		"server.url":          cfg.Server.BaseUrl,
		"server.upload-token": maskToken(cfg.Server.UploadToken),
		"server.ca-file":      cfg.Server.CaFile,
		"output":              cfg.Output,
		"retries":             cfg.Retries,
		"timeout":             cfg.Timeout,
		"no-color":            cfg.NoColor,
		"verbose":             cfg.Verbose,
		"quiet":               cfg.Quiet,
	}
	view := configView{Settings: []setting{{"config", orNil(cfg.File()), cfg.Source("config")}}}
	for _, k := range config.Keys {
		val := values[k]
		if s, ok := val.(string); ok {
			val = orNil(s)
		}
		view.Settings = append(view.Settings, setting{k, val, cfg.Source(k)})
	}
	return view
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (c configView) printText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SETTING\tVALUE\tSOURCE")
	for _, s := range c.Settings {
		val := "-"
		if s.Value != nil {
			val = fmt.Sprint(s.Value)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, val, s.Source)
	}
	return tw.Flush()
}
