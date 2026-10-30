package logging

import (
	"log/slog"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"", Text, false},
		{"   ", Text, false},
		{"text", Text, false},
		{"json", JSON, false},
		{" JSON ", JSON, false},
		{"Text", Text, false},
		{"yaml", "", true},
		{"table", "", true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseFormat(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseFormat(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.in) {
				t.Errorf("error %q does not name the value %q", err, tc.in)
			}
		})
	}
}

func TestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want []string // substrings of the error, none for no error
	}{
		{"zero value", Options{}, nil},
		{"json verbose", Options{Format: JSON, Verbose: true}, nil},
		{"text quiet", Options{Format: Text, Quiet: true}, nil},
		{"bad format", Options{Format: "xml"}, []string{`"xml"`}},
		{"quiet and verbose", Options{Quiet: true, Verbose: true}, []string{"--quiet and --verbose"}},
		{"both errors", Options{Format: "xml", Quiet: true, Verbose: true}, []string{`"xml"`, "--quiet and --verbose"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Validate() = %q, want it to contain %q", err, w)
				}
			}
		})
	}
}

func TestOptionsLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want slog.Level
	}{
		{"default", Options{}, slog.LevelInfo},
		{"verbose", Options{Verbose: true}, slog.LevelDebug},
		{"quiet", Options{Quiet: true}, slog.LevelWarn},
		{"verbose wins", Options{Verbose: true, Quiet: true}, slog.LevelDebug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.Level(); got != tc.want {
				t.Errorf("Level() = %v, want %v", got, tc.want)
			}
		})
	}
}
