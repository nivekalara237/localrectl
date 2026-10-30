package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Format is how a command prints its result on stdout.
type Format string

const (
	Text Format = "text"
	JSON Format = "json"
)

// ParseFormat reads a --output value. Case and surrounding spaces are ignored, and an empty
// value is the default, text.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case "":
		return Text, nil
	case Text, JSON:
		return f, nil
	default:
		return "", fmt.Errorf("output %q: must be one of 'text' and 'json'", s)
	}
}

// Options are the output settings shared by every command.
type Options struct {
	Format  Format
	Verbose bool
	Quiet   bool
	NoColor bool
}

// Validate refuses an unknown format and --quiet with --verbose.
func (o Options) Validate() error {
	var errs []error
	if _, err := ParseFormat(string(o.Format)); err != nil {
		errs = append(errs, err)
	}
	if o.Quiet && o.Verbose {
		errs = append(errs, errors.New("--quiet and --verbose cannot be used together"))
	}
	return errors.Join(errs...)
}

// Level is the lowest level the logger prints: Debug with --verbose, Warn with --quiet.
func (o Options) Level() slog.Level {
	switch {
	case o.Verbose:
		return slog.LevelDebug
	case o.Quiet:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
