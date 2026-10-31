package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

const token = "abcdefghijklmnopqrstuvwxyz"

var errWrite = errors.New("write failed")

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

// isolate runs the test in an empty directory, which is also $HOME, with no LOCAL_REGISTRY_*
// or NO_COLOR variable: the developer's ~/.lr/config must not change the result.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "LOCAL_REGISTRY_") || k == "NO_COLOR" {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
	return dir
}

func writeConfig(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

type result struct {
	stdout, stderr string
	cmd            *cobra.Command
	err            error
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	root := NewRootCommand()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errb)
	cmd, err := root.ExecuteC()
	return result{out.String(), errb.String(), cmd, err}
}

func mustRun(t *testing.T, args ...string) result {
	t.Helper()
	r := run(t, args...)
	if r.err != nil {
		t.Fatalf("lrctl %s: %v\nstderr: %s", strings.Join(args, " "), r.err, r.stderr)
	}
	return r
}

func secretFile(t *testing.T, mode os.FileMode) string {
	return writeConfig(t, "server:\n  url: https://r.example.com\n  upload-token: \""+token+"\"\nretries: 5\n", mode)
}

func TestRootWithoutCommand(t *testing.T) {
	isolate(t)
	r := mustRun(t)
	if r.stdout != "" || r.stderr != "" {
		t.Errorf("stdout %q, stderr %q, want nothing", r.stdout, r.stderr)
	}
}

func TestConfigText(t *testing.T) {
	isolate(t)
	file := secretFile(t, 0o600)
	r := mustRun(t, "config", "--config", file)

	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2+len(config.Keys) {
		t.Fatalf("%d lines, want a header and %d settings:\n%s", len(lines), 1+len(config.Keys), r.stdout)
	}
	for i, want := range [][]string{
		{"SETTING", "VALUE", "SOURCE"},
		{"config", file, "flag"},
		{"server.url", "https://r.example.com", "config", "file"},
		{"server.upload-token", "abcdefg*****", "config", "file"},
		{"server.ca-file", "-", "default"},
		{"output", "text", "default"},
		{"retries", "5", "config", "file"},
	} {
		if got := strings.Fields(lines[i]); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
	}
	if strings.Contains(r.stdout+r.stderr, token) {
		t.Error("the token is printed")
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want nothing for a 0600 file", r.stderr)
	}
}

type configJSON struct {
	Settings []struct {
		Name   string `json:"name"`
		Value  any    `json:"value"`
		Source string `json:"source"`
	} `json:"settings"`
}

func (c configJSON) get(name string) (any, string) {
	for _, s := range c.Settings {
		if s.Name == name {
			return s.Value, s.Source
		}
	}
	return "missing", "missing"
}

func TestConfigJSON(t *testing.T) {
	isolate(t)
	r := mustRun(t, "config", "--config", secretFile(t, 0o600), "-o", "json", "--timeout", "10")

	var got configJSON
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if len(got.Settings) != 1+len(config.Keys) {
		t.Errorf("%d settings, want %d", len(got.Settings), 1+len(config.Keys))
	}
	for _, tc := range []struct {
		name   string
		value  any
		source string
	}{
		{"server.upload-token", "abcdefg*****", "config file"},
		{"server.ca-file", nil, "default"},
		{"output", "json", "flag"},
		{"retries", float64(5), "config file"},
		{"timeout", float64(10), "flag"},
		{"no-color", false, "default"},
	} {
		if v, src := got.get(tc.name); v != tc.value || src != tc.source {
			t.Errorf("%s = %v from %q, want %v from %q", tc.name, v, src, tc.value, tc.source)
		}
	}
	if strings.Contains(r.stdout, token) {
		t.Error("the token is printed")
	}
}

func TestConfigFoundBySearch(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("retries: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got configJSON
	if err := json.Unmarshal([]byte(mustRun(t, "config", "-o", "json").stdout), &got); err != nil {
		t.Fatal(err)
	}
	if v, src := got.get("config"); v != "config.yaml" || src != "default" {
		t.Errorf("config = %v from %q, want config.yaml from default", v, src)
	}
	if v, _ := got.get("retries"); v != float64(2) {
		t.Errorf("retries = %v, want 2", v)
	}
}

func TestConfigWithoutFile(t *testing.T) {
	isolate(t)
	var got configJSON
	if err := json.Unmarshal([]byte(mustRun(t, "config", "-o", "json").stdout), &got); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "server.upload-token"} {
		if v, src := got.get(name); v != nil || src != "default" {
			t.Errorf("%s = %v from %q, want null from default", name, v, src)
		}
	}
}

func TestVerbose(t *testing.T) {
	isolate(t)
	r := mustRun(t, "config", "--config", secretFile(t, 0o600), "-o", "json", "-v")

	if !json.Valid([]byte(r.stdout)) {
		t.Fatalf("stdout is not one JSON document: %s", r.stdout)
	}
	var settings int
	for line := range strings.SplitSeq(strings.TrimSpace(r.stderr), "\n") {
		var rec struct{ Level, Msg string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		if rec.Level == "DEBUG" && rec.Msg == "setting" {
			settings++
		}
	}
	if settings != len(config.Keys) {
		t.Errorf("%d 'setting' records, want %d:\n%s", settings, len(config.Keys), r.stderr)
	}
	if !strings.Contains(r.stderr, `"msg":"config file"`) {
		t.Errorf("stderr does not name the config file:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, token) {
		t.Error("the token is logged")
	}
}

func TestQuietAndWarnings(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		mode os.FileMode
		want []string // on stderr; none for an empty stderr
	}{
		{"readable token, quiet", []string{"-q"}, 0o644, []string{"level=WARN", "chmod 600"}},
		{"readable token, json", []string{"-o", "json"}, 0o644, []string{`"level":"WARN"`}},
		{"private token, quiet", []string{"-q"}, 0o600, nil},
		{"private token, verbose", []string{"-v"}, 0o600, []string{"level=DEBUG"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			r := mustRun(t, append([]string{"config", "--config", secretFile(t, tc.mode)}, tc.args...)...)
			if len(tc.want) == 0 && r.stderr != "" {
				t.Errorf("stderr = %q, want nothing", r.stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.stderr, w) {
					t.Errorf("stderr %q does not contain %q", r.stderr, w)
				}
			}
			if strings.Contains(r.stdout, "level=") {
				t.Errorf("a log is on stdout: %q", r.stdout)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"quiet and verbose", []string{"config", "-q", "-v"}, "--quiet and --verbose"},
		{"bad output", []string{"config", "-o", "yaml"}, `output "yaml"`},
		{"missing config file", []string{"config", "--config", "nope.yaml"}, "nope.yaml"},
		{"unknown flag", []string{"config", "--bad"}, "unknown flag: --bad (see 'lrctl config --help')"},
		{"bad flag value", []string{"--retries", "x"}, "(see 'lrctl --help')"},
		{"argument to config", []string{"config", "extra"}, `unexpected argument "extra" for "lrctl config"`},
		{"argument to version", []string{"version", "extra"}, `unexpected argument "extra" for "lrctl version"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			r := run(t, tc.args...)
			if r.err == nil {
				t.Fatal("no error")
			}
			if !config.IsUsageError(r.err) {
				t.Errorf("%v is not a usage error", r.err)
			}
			if !strings.Contains(r.err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", r.err, tc.want)
			}
			if r.stdout != "" || strings.Contains(r.stderr, "Usage:") {
				t.Errorf("stdout %q, stderr %q: the whole usage must not be printed", r.stdout, r.stderr)
			}
		})
	}
}

// main prints the error with the Printer stored in the context of the command that failed.
func TestFailedRunKeepsThePrinter(t *testing.T) {
	isolate(t)
	r := run(t, "config", "-o", "json", "--config", "nope.yaml")
	if r.err == nil {
		t.Fatal("no error")
	}
	p := logging.From(r.cmd.Context())
	if p.Format != logging.JSON {
		t.Fatalf("printer format = %q, want json", p.Format)
	}
	if config.From(r.cmd.Context()) != nil {
		t.Error("a refused configuration is stored")
	}

	var errb bytes.Buffer
	p.Err = &errb
	p.Error(r.err)
	var rec struct{ Error string }
	if err := json.Unmarshal(errb.Bytes(), &rec); err != nil || !strings.Contains(rec.Error, "nope.yaml") {
		t.Errorf("error printed as %q", errb.String())
	}
}

// Every run builds its own configuration: nothing is left by the previous one.
func TestRunsAreIsolated(t *testing.T) {
	isolate(t)
	file := secretFile(t, 0o600)
	mustRun(t, "config", "--config", file)

	var got configJSON
	if err := json.Unmarshal([]byte(mustRun(t, "config", "-o", "json").stdout), &got); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]any{"config": nil, "retries": float64(3), "server.upload-token": nil} {
		if v, src := got.get(name); v != want || src != "default" {
			t.Errorf("%s = %v from %q after a run with %s: it leaked", name, v, src, file)
		}
	}
}

func TestConfigCommandWithoutRoot(t *testing.T) {
	cmd := newConfigCommand()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Execute() = %v, want a 'not loaded' error", err)
	}
}

func TestConfigOutputFails(t *testing.T) {
	isolate(t)
	root := NewRootCommand()
	root.SetArgs([]string{"config"})
	root.SetOut(failWriter{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); !errors.Is(err, errWrite) {
		t.Errorf("Execute() = %v, want %v", err, errWrite)
	}
}

func TestVersion(t *testing.T) {
	isolate(t)
	t.Run("text", func(t *testing.T) {
		r := mustRun(t, "version")
		if !strings.HasPrefix(r.stdout, "lrctl ") || !strings.Contains(r.stdout, runtime.Version()) ||
			strings.Count(r.stdout, "\n") != 1 {
			t.Errorf("stdout = %q", r.stdout)
		}
	})
	t.Run("alias and json", func(t *testing.T) {
		r := mustRun(t, "v", "-o", "json")
		var got map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
		}
		if got["name"] != "lrctl" || got["go_version"] != runtime.Version() ||
			got["platform"] != runtime.GOOS+"/"+runtime.GOARCH {
			t.Errorf("version = %v", got)
		}
		for _, k := range []string{"version", "commit", "date", "dirty"} {
			if _, ok := got[k]; !ok {
				t.Errorf("no %q in %v", k, got)
			}
		}
	})
	t.Run("output fails", func(t *testing.T) {
		root := NewRootCommand()
		root.SetArgs([]string{"version"})
		root.SetOut(failWriter{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); !errors.Is(err, errWrite) {
			t.Errorf("Execute() = %v, want %v", err, errWrite)
		}
	})
}

func TestNoArgs(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	if err := noArgs(cmd, nil); err != nil {
		t.Errorf("noArgs(nil) = %v", err)
	}
	if err := noArgs(cmd, []string{"a", "b"}); !config.IsUsageError(err) || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("noArgs(a, b) = %v, want a usage error naming the first one", err)
	}
}

func TestMaskToken(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"abc":                "*****",
		"abcdefghijklm":      "*****", // 13 characters: too short to show any
		"abcdefghijklmn":     "abcdefg*****",
		token:                "abcdefg*****",
		"0123456789abcdefgh": "0123456*****",
	} {
		if got := maskToken(in); got != want {
			t.Errorf("maskToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrNil(t *testing.T) {
	if orNil("") != nil || orNil("x") != "x" {
		t.Error("orNil")
	}
}

func TestPrintTextFails(t *testing.T) {
	v := configView{Settings: []setting{{"a", nil, "default"}}}
	if err := v.printText(failWriter{}); !errors.Is(err, errWrite) {
		t.Errorf("printText() = %v, want %v", err, errWrite)
	}
	var buf bytes.Buffer
	if err := v.printText(&buf); err != nil || strings.Join(strings.Fields(buf.String()), " ") != "SETTING VALUE SOURCE a - default" {
		t.Errorf("printText() = %v, %q", err, buf.String())
	}
}

// exit-code applies to the failure of any command; a usage error stays 2.
func TestExitCode(t *testing.T) {
	srv, _ := schemaServer(t, http.StatusOK, productSchema)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	for _, tc := range []struct {
		name string
		env  string // LOCAL_REGISTRY_EXIT_CODE
		file string // content of the config file
		args []string
		want int
	}{
		{"success", "", "", []string{"version"}, 0},
		{"default", "", "", []string{"ping", "--server-url", closed.URL}, 1},
		{"flag on ping", "", "", []string{"ping", "--server-url", closed.URL, "--exit-code", "3"}, 3},
		{"flag on validate", "", "", []string{"validate", "--server-url", srv.URL, "registry.json", "--exit-code", "4"}, 4},
		{"environment", "5", "", []string{"ping", "--server-url", closed.URL}, 5},
		{"config file", "", "exit-code: 6\n", []string{"ping", "--server-url", closed.URL}, 6},
		{"0 never fails", "", "", []string{"ping", "--server-url", closed.URL, "--exit-code", "0"}, 0},
		{"usage error", "", "", []string{"ping", "extra", "--exit-code", "7"}, 2},
		{"unknown flag", "", "", []string{"ping", "--nope", "--exit-code", "7"}, 2},
		{"exit code out of range", "", "", []string{"ping", "--exit-code", "200"}, 2},
		{"missing config file", "", "", []string{"ping", "--config", "nope.yaml", "--exit-code", "7"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			args := tc.args
			if tc.env != "" {
				t.Setenv("LOCAL_REGISTRY_EXIT_CODE", tc.env)
			}
			if tc.file != "" {
				args = append(args, "--config", writeConfig(t, tc.file, 0o600))
			}
			r := run(t, args...)
			if got := ExitCode(r.cmd, r.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", r.err, got, tc.want)
			}
		})
	}

	// Without a loaded configuration, there is no exit-code setting.
	bare := newPingCommand()
	bare.SetContext(context.Background())
	for _, cmd := range []*cobra.Command{nil, bare} {
		if got := ExitCode(cmd, errors.New("x")); got != 1 {
			t.Errorf("ExitCode(%v, x) = %d, want 1", cmd, got)
		}
	}
}

// A command must not declare its own --exit-code: it would hide the global one from LoadConfig.
func TestExitCodeNotShadowed(t *testing.T) {
	for _, c := range NewRootCommand().Commands() {
		if c.LocalNonPersistentFlags().Lookup("exit-code") != nil {
			t.Errorf("%s declares its own --exit-code", c.Name())
		}
	}
}
