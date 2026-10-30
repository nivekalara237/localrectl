package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

var errWrite = errors.New("write failed")

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

func newTestPrinter(opts Options) (p *Printer, out, errw *bytes.Buffer) {
	out, errw = &bytes.Buffer{}, &bytes.Buffer{}
	return NewPrinter(out, errw, opts), out, errw
}

func TestNewPrinter(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want Format
	}{
		{"empty is text", Options{}, Text},
		{"json", Options{Format: JSON, NoColor: true}, JSON},
		{"spelled loosely", Options{Format: " Json"}, JSON},
		{"invalid is text", Options{Format: "xml"}, Text},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, out, errw := newTestPrinter(tc.opts)
			if p.Format != tc.want {
				t.Errorf("Format = %q, want %q", p.Format, tc.want)
			}
			if p.NoColor != tc.opts.NoColor {
				t.Errorf("NoColor = %v, want %v", p.NoColor, tc.opts.NoColor)
			}
			if p.Out != out || p.Err != errw {
				t.Error("Out and Err are not the writers given")
			}
			if p.Logger() == nil {
				t.Fatal("Logger() = nil")
			}
		})
	}
}

func TestPrinterLoggerWritesOnErr(t *testing.T) {
	p, out, errw := newTestPrinter(Options{Format: JSON})
	p.Logger().Info("hello", "n", 1)

	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: logs go on stderr", out)
	}
	var rec map[string]any
	if err := json.Unmarshal(errw.Bytes(), &rec); err != nil {
		t.Fatalf("stderr %q is not one JSON record: %v", errw, err)
	}
	if rec["msg"] != "hello" || rec["n"] != float64(1) {
		t.Errorf("record = %v", rec)
	}
}

type result struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestPrinterResult(t *testing.T) {
	v := result{Name: "lrctl", Count: 2}
	textFn := func(w io.Writer) error {
		_, err := io.WriteString(w, "custom text\n")
		return err
	}

	for _, tc := range []struct {
		name   string
		format Format
		text   func(io.Writer) error
		want   string
	}{
		{"json ignores text", JSON, textFn, "{\n  \"name\": \"lrctl\",\n  \"count\": 2\n}\n"},
		{"json without text", JSON, nil, "{\n  \"name\": \"lrctl\",\n  \"count\": 2\n}\n"},
		{"text uses text", Text, textFn, "custom text\n"},
		{"text without text", Text, nil, "{lrctl 2}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, out, errw := newTestPrinter(Options{Format: tc.format})
			if err := p.Result(v, tc.text); err != nil {
				t.Fatalf("Result() = %v", err)
			}
			if out.String() != tc.want {
				t.Errorf("stdout = %q, want %q", out, tc.want)
			}
			if errw.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", errw)
			}
		})
	}
}

func TestPrinterResultErrors(t *testing.T) {
	t.Run("json value that cannot be encoded", func(t *testing.T) {
		p, _, _ := newTestPrinter(Options{Format: JSON})
		var ue *json.UnsupportedTypeError
		if err := p.Result(make(chan int), nil); !errors.As(err, &ue) {
			t.Errorf("Result() = %v, want a *json.UnsupportedTypeError", err)
		}
	})
	t.Run("text writer fails", func(t *testing.T) {
		p := NewPrinter(failWriter{}, io.Discard, Options{Format: Text})
		if err := p.Result("x", nil); !errors.Is(err, errWrite) {
			t.Errorf("Result() = %v, want %v", err, errWrite)
		}
	})
	t.Run("text function fails", func(t *testing.T) {
		p, _, _ := newTestPrinter(Options{Format: Text})
		want := errors.New("boom")
		if err := p.Result("x", func(io.Writer) error { return want }); !errors.Is(err, want) {
			t.Errorf("Result() = %v, want %v", err, want)
		}
	})
}

func TestPrinterError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format Format
		err    error
		want   string
	}{
		{"nil prints nothing", Text, nil, ""},
		{"nil prints nothing in json", JSON, nil, ""},
		{"text", Text, errors.New(`config file c.yaml: unknown setting "x"`), "config file c.yaml: unknown setting \"x\"\n"},
		{"json", JSON, errors.New(`unknown setting "x"`), `{"error":"unknown setting \"x\""}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, out, errw := newTestPrinter(Options{Format: tc.format})
			p.Error(tc.err)
			if errw.String() != tc.want {
				t.Errorf("stderr = %q, want %q", errw, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want nothing: errors go on stderr", out)
			}
		})
	}
}

func TestPrinterUseColor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    Options
		noColor string
		want    bool
	}{
		{"text", Options{Format: Text}, "", true},
		{"json is never colored", Options{Format: JSON}, "", false},
		{"--no-color", Options{Format: Text, NoColor: true}, "", false},
		{"NO_COLOR set", Options{Format: Text}, "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			p, _, _ := newTestPrinter(tc.opts)
			if got := p.UseColor(); got != tc.want {
				t.Errorf("UseColor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithAndFrom(t *testing.T) {
	t.Run("stored printer", func(t *testing.T) {
		p, _, _ := newTestPrinter(Options{Format: JSON})
		if got := From(With(context.Background(), p)); got != p {
			t.Errorf("From() = %p, want the stored %p", got, p)
		}
	})

	fallback := func(t *testing.T, got *Printer) {
		t.Helper()
		if got == nil {
			t.Fatal("From() = nil, want a fallback printer")
		}
		if got.Format != Text || got.Out != os.Stdout || got.Err != os.Stderr || got.Logger() == nil {
			t.Errorf("fallback = %+v, want text on stdout and stderr", got)
		}
	}
	t.Run("nothing stored", func(t *testing.T) {
		fallback(t, From(context.Background()))
	})
	t.Run("nil printer stored", func(t *testing.T) {
		fallback(t, From(With(context.Background(), nil)))
	})
	t.Run("nil context", func(t *testing.T) {
		var ctx context.Context
		fallback(t, From(ctx))
	})
}

func TestAddLogArgs(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	AddLogArgs(fs)

	f := fs.Lookup("output")
	if f == nil {
		t.Fatal("no --output flag")
	}
	if f.Shorthand != "o" || f.DefValue != string(Text) {
		t.Errorf("--output: shorthand %q, default %q, want o and text", f.Shorthand, f.DefValue)
	}
	if err := fs.Parse([]string{"-o", "json"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := fs.GetString("output"); got != "json" {
		t.Errorf("-o json gives %q", got)
	}
}

func TestAddArgs(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	AddArgs(fs)

	for name, short := range map[string]string{"verbose": "v", "quiet": "q"} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("no --%s flag", name)
		}
		if f.Shorthand != short || f.DefValue != "false" {
			t.Errorf("--%s: shorthand %q, default %q, want %s and false", name, f.Shorthand, f.DefValue, short)
		}
	}
	if err := fs.Parse([]string{"-v", "-q"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"verbose", "quiet"} {
		if got, _ := fs.GetBool(name); !got {
			t.Errorf("--%s not set by its shorthand", name)
		}
	}
	if !strings.Contains(fs.FlagUsages(), "stderr") {
		t.Error("--verbose usage does not say where the details go")
	}
}
