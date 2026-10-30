package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerFormat(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		NewLogger(&buf, Options{Format: JSON}).Info("config file", "path", "c.yaml")

		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("%q is not a JSON record: %v", buf.String(), err)
		}
		if rec["level"] != "INFO" || rec["msg"] != "config file" || rec["path"] != "c.yaml" {
			t.Errorf("record = %v", rec)
		}
	})
	for _, f := range []Format{Text, "", "xml"} {
		t.Run("text for "+string(f), func(t *testing.T) {
			var buf bytes.Buffer
			NewLogger(&buf, Options{Format: f}).Info("config file", "path", "c.yaml")

			got := buf.String()
			if !strings.Contains(got, "level=INFO") || !strings.Contains(got, `msg="config file"`) ||
				!strings.Contains(got, "path=c.yaml") {
				t.Errorf("text record = %q", got)
			}
		})
	}
}

func TestNewLoggerLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want []string // the levels written, in order
	}{
		{"default", Options{}, []string{"INFO", "WARN", "ERROR"}},
		{"verbose", Options{Verbose: true}, []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{"quiet", Options{Quiet: true}, []string{"WARN", "ERROR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewLogger(&buf, tc.opts)
			l.Debug("m")
			l.Info("m")
			l.Warn("m")
			l.Error("m")

			if got := levels(t, &buf); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("levels written = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewLoggerRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, Options{Format: JSON}).Info("request",
		"upload-token", "8888800000000000000000000001111111111188888888888888",
		"Authorization", "Bearer abc",
		"PASSWORD", "hunter2",
		slog.Group("server", "upload_token", "s3cr3t", "url", "https://registry.example.com"),
		"url", "https://registry.example.com",
	)

	out := buf.String()
	for _, secret := range []string{"88888", "Bearer abc", "hunter2", "s3cr3t"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds the secret %q: %s", secret, out)
		}
	}

	var rec struct {
		Token  string `json:"upload-token"`
		Auth   string `json:"Authorization"`
		Pass   string `json:"PASSWORD"`
		URL    string `json:"url"`
		Server struct {
			Token string `json:"upload_token"`
			URL   string `json:"url"`
		} `json:"server"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"upload-token": rec.Token, "Authorization": rec.Auth, "PASSWORD": rec.Pass, "server.upload_token": rec.Server.Token,
	} {
		if got != Redacted {
			t.Errorf("%s = %q, want %q", name, got, Redacted)
		}
	}
	if rec.URL != "https://registry.example.com" || rec.Server.URL != rec.URL {
		t.Errorf("url = %q, server.url = %q: a plain value must be kept", rec.URL, rec.Server.URL)
	}
}

func TestNotes(t *testing.T) {
	var n Notes
	n.Debug("searched", "dirs", 3)
	n.Warn("readable by other users", "file", "c.yaml")
	n.Add(slog.LevelInfo, "config file", "path", "c.yaml")
	n.Add(slog.LevelError, "no args")

	want := []Note{
		{slog.LevelDebug, "searched", []any{"dirs", 3}},
		{slog.LevelWarn, "readable by other users", []any{"file", "c.yaml"}},
		{slog.LevelInfo, "config file", []any{"path", "c.yaml"}},
		{slog.LevelError, "no args", nil},
	}
	if len(n) != len(want) {
		t.Fatalf("len(notes) = %d, want %d", len(n), len(want))
	}
	for i, w := range want {
		if n[i].Level != w.Level || n[i].Msg != w.Msg || len(n[i].Args) != len(w.Args) {
			t.Errorf("notes[%d] = %+v, want %+v", i, n[i], w)
		}
	}

	var buf bytes.Buffer
	n.Flush(NewLogger(&buf, Options{Format: JSON, Quiet: true}))

	// --quiet keeps the warning and the error only, in their order.
	if got := strings.Join(levels(t, &buf), ","); got != "WARN,ERROR" {
		t.Errorf("flushed levels = %s, want WARN,ERROR", got)
	}
	if !strings.Contains(buf.String(), `"file":"c.yaml"`) {
		t.Errorf("the arguments of a note are lost: %s", buf.String())
	}
	if n != nil {
		t.Errorf("notes after Flush = %v, want nil", n)
	}

	buf.Reset()
	n.Flush(NewLogger(&buf, Options{Format: JSON}))
	if buf.Len() != 0 {
		t.Errorf("a second Flush wrote %q", buf.String())
	}
}

// levels returns the level of each JSON or text record in buf.
func levels(t *testing.T, buf *bytes.Buffer) (got []string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct{ Level string }
		if json.Unmarshal([]byte(line), &rec) == nil {
			got = append(got, rec.Level)
			continue
		}
		for _, f := range strings.Fields(line) {
			if l, ok := strings.CutPrefix(f, "level="); ok {
				got = append(got, l)
			}
		}
	}
	return got
}
