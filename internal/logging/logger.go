package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/pflag"
)

// Printer writes the result of a command on Out, and everything else (logs, warnings,
// errors) on Err, so that `lrctl <command> <args> -o json | jq` always reads a single JSON document.
type Printer struct {
	NoColor  bool
	Format   Format
	Err, Out io.Writer

	log *slog.Logger
}

// NewPrinter returns a Printer for opts, with its logger on errw. An invalid format is text:
// opts.Validate is the place to refuse it.
func NewPrinter(out, errw io.Writer, opts Options) *Printer {
	f, err := ParseFormat(string(opts.Format))
	if err != nil {
		f = Text
	}
	opts.Format = f
	return &Printer{
		NoColor: opts.NoColor,
		Format:  f,
		Err:     errw,
		Out:     out,
		log:     NewLogger(errw, opts),
	}
}

// Logger is the logger that writes on Err, at the level given by the options.
func (p *Printer) Logger() *slog.Logger {
	return p.log
}

// Result prints v: as indented JSON, or with text when the format is text. A nil text prints v
// with fmt.
func (p *Printer) Result(v any, text func(w io.Writer) error) error {
	if p.Format == JSON {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if text == nil {
		_, err := fmt.Fprintln(p.Out, v)
		return err
	}
	return text(p.Out)
}

// Error prints err on Err: {"error": "..."} in JSON, the message alone in text.
func (p *Printer) Error(err error) {
	if err == nil {
		return
	}
	if p.Format == JSON {
		_ = json.NewEncoder(p.Err).Encode(struct {
			Error string `json:"error"`
		}{err.Error()})
		return
	}
	_, _ = fmt.Fprintln(p.Err, err)
}

// UseColor tells whether text output may be colored: never in JSON, nor with --no-color or a
// non-empty NO_COLOR (https://no-color.org).
func (p *Printer) UseColor() bool {
	return p.Format == Text && !p.NoColor && os.Getenv("NO_COLOR") == ""
}

type printerKey struct{}

// With stores p in ctx, for the commands to find it with From.
func With(ctx context.Context, p *Printer) context.Context {
	return context.WithValue(ctx, printerKey{}, p)
}

// From returns the Printer stored by With, or a text Printer on stdout and stderr: a command
// run without the root's PersistentPreRunE (a test) still prints.
func From(ctx context.Context) *Printer {
	if ctx != nil {
		if p, ok := ctx.Value(printerKey{}).(*Printer); ok && p != nil {
			return p
		}
	}
	return NewPrinter(os.Stdout, os.Stderr, Options{Format: Text})
}

func AddLogArgs(fs *pflag.FlagSet) {
	fs.StringP("output", "o", string(Text), "Output format: text or json")
}
