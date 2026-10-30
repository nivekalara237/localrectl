package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/pflag"
)

func AddArgs(fs *pflag.FlagSet) {
	fs.BoolP("verbose", "v", false, "Print details on stderr (config file used, requests…)")
	fs.BoolP("quiet", "q", false, "Print only the result and the errors")
}

// Redacted replaces the value of a secret attribute.
const Redacted = "[REDACTED]"

// NewLogger writes on w at opts.Level(), in JSON with -o json so that a CI can parse it too.
// The value of an attribute named like a secret (token, authorization, password) is never
// written.
func NewLogger(w io.Writer, opts Options) *slog.Logger {
	ho := &slog.HandlerOptions{Level: opts.Level(), ReplaceAttr: redact}
	if opts.Format == JSON {
		return slog.New(slog.NewJSONHandler(w, ho))
	}
	return slog.New(slog.NewTextHandler(w, ho))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	for _, s := range []string{"token", "upload-token", "secret", "authorization", "password"} {
		if strings.Contains(k, s) {
			return slog.String(a.Key, Redacted)
		}
	}
	return a
}

// Note is a message kept until the logger exists: the configuration is loaded before
// --verbose is known, since the file can set it.
type Note struct {
	Level slog.Level
	Msg   string
	Args  []any
}

// Notes keeps messages to write later with Flush.
type Notes []Note

func (n *Notes) Add(level slog.Level, msg string, args ...any) {
	*n = append(*n, Note{Level: level, Msg: msg, Args: args})
}

func (n *Notes) Debug(msg string, args ...any) { n.Add(slog.LevelDebug, msg, args...) }
func (n *Notes) Warn(msg string, args ...any)  { n.Add(slog.LevelWarn, msg, args...) }

// Flush writes the notes on l, in their order, and empties them.
func (n *Notes) Flush(l *slog.Logger) {
	for _, note := range *n {
		l.Log(context.Background(), note.Level, note.Msg, note.Args...)
	}
	*n = nil
}
