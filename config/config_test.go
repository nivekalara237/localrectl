package config

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/pflag"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, envPrefix+"_") {
			t.Setenv(k, "") // restored after the test
			_ = os.Unsetenv(k)
		}
	}
	saved := configSearchDirs
	configSearchDirs = []string{AppHomeDir, "."}
	t.Cleanup(func() { configSearchDirs = saved })
	return dir
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile applies the umask
		t.Fatal(err)
	}
	return path
}

func newFlags(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("lrctl", pflag.ContinueOnError)
	AddFlags(fs)
	logging.AddArgs(fs)
	logging.AddLogArgs(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs
}

func load(t *testing.T, args ...string) (*Config, logging.Notes, error) {
	t.Helper()
	var notes logging.Notes
	c, err := LoadConfig(newFlags(t, args...), &notes)
	if c == nil {
		t.Fatal("LoadConfig returned a nil Config")
	}
	return c, notes, err
}

func mustLoad(t *testing.T, args ...string) (*Config, logging.Notes) {
	t.Helper()
	c, notes, err := load(t, args...)
	if err != nil {
		t.Fatalf("LoadConfig(%q) = %v", args, err)
	}
	return c, notes
}

func findNote(notes logging.Notes, level slog.Level, msg string) (logging.Note, bool) {
	for _, n := range notes {
		if n.Level == level && n.Msg == msg {
			return n, true
		}
	}
	return logging.Note{}, false
}

func noteArg(n logging.Note, key string) any {
	for i := 0; i+1 < len(n.Args); i += 2 {
		if n.Args[i] == key {
			return n.Args[i+1]
		}
	}
	return nil
}

const fullFile = `server:
  url: https://registry.example.com/
  upload-token: "0123456789abcdef"
  ca-file: /etc/ssl/ca.pem
timeout: 60
retries: 5
output: json
no-color: true
verbose: true
exit-code: 4
`

func TestLoadConfigDefaults(t *testing.T) {
	dir := isolate(t)
	c, notes := mustLoad(t)

	want := AppUntypedConfig{
		Server:   ServerConf{BaseUrl: "http://localhost:8077"},
		Timeout:  300,
		Retries:  3,
		Output:   "text",
		ExitCode: 1,
	}
	if c.AppUntypedConfig != want {
		t.Errorf("config = %+v, want %+v", c.AppUntypedConfig, want)
	}
	if c.File() != "" {
		t.Errorf("File() = %q, want none", c.File())
	}
	for _, k := range append([]string{"config"}, Keys...) {
		if got := c.Source(k); got != "default" {
			t.Errorf("Source(%q) = %q, want default", k, got)
		}
	}

	n, ok := findNote(notes, slog.LevelDebug, "no config file")
	if !ok {
		t.Fatalf("no 'no config file' note in %v", notes)
	}
	if searched := noteArg(n, "searched"); searched != filepath.Join(dir, ".lr")+", ." {
		t.Errorf("searched = %v", searched)
	}
	var settings int
	for _, n := range notes {
		if n.Msg == "setting" {
			settings++
		}
	}
	if settings != len(Keys) {
		t.Errorf("%d 'setting' notes, want one per key (%d)", settings, len(Keys))
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := isolate(t)
	file := writeFile(t, filepath.Join(dir, "c.yaml"), fullFile, 0o600)
	c, notes := mustLoad(t, "--config", file)

	want := AppUntypedConfig{
		Server: ServerConf{
			BaseUrl:     "https://registry.example.com",
			UploadToken: "0123456789abcdef",
			CaFile:      "/etc/ssl/ca.pem",
		},
		Timeout:  60,
		Retries:  5,
		Verbose:  true,
		NoColor:  true,
		Output:   "json",
		ExitCode: 4,
	}
	if c.AppUntypedConfig != want {
		t.Errorf("config = %+v, want %+v", c.AppUntypedConfig, want)
	}
	if c.File() != file {
		t.Errorf("File() = %q, want %q", c.File(), file)
	}
	if got := c.Source("config"); got != "flag" {
		t.Errorf("Source(config) = %q, want flag", got)
	}
	for _, k := range Keys {
		wantSrc := "config file"
		if k == "quiet" {
			wantSrc = "default"
		}
		if got := c.Source(k); got != wantSrc {
			t.Errorf("Source(%q) = %q, want %q", k, got, wantSrc)
		}
	}
	if n, ok := findNote(notes, slog.LevelDebug, "config file"); !ok || noteArg(n, "path") != file {
		t.Errorf("no 'config file' note for %s in %v", file, notes)
	}
	if _, ok := findNote(notes, slog.LevelWarn, "the config file holds the upload token but is readable by other users"); ok {
		t.Error("warning for a 0600 file")
	}
	for _, n := range notes {
		for _, a := range n.Args {
			if a == "0123456789abcdef" {
				t.Errorf("note %+v holds the token", n)
			}
		}
	}
}

func TestLoadConfigPrecedence(t *testing.T) {
	const file = "retries: 5\nserver:\n  url: https://file.example.com\n"
	for _, tc := range []struct {
		name       string
		args       []string
		env        map[string]string
		retries    int
		retriesSrc string
		url        string
		urlSrc     string
	}{
		{"file", nil, nil, 5, "config file", "https://file.example.com", "config file"},
		{"env over file",
			nil, map[string]string{"LOCAL_REGISTRY_RETRIES": "9", "LOCAL_REGISTRY_URL": "https://env.example.com"},
			9, "env LOCAL_REGISTRY_RETRIES", "https://env.example.com", "env LOCAL_REGISTRY_URL"},
		{"flag over env",
			[]string{"--retries", "7", "--server-url", "https://flag.example.com"},
			map[string]string{"LOCAL_REGISTRY_RETRIES": "9", "LOCAL_REGISTRY_URL": "https://env.example.com"},
			7, "flag", "https://flag.example.com", "flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolate(t)
			path := writeFile(t, filepath.Join(dir, "c.yaml"), file, 0o600)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, _ := mustLoad(t, append([]string{"--config", path}, tc.args...)...)

			if c.Retries != tc.retries || c.Source("retries") != tc.retriesSrc {
				t.Errorf("retries = %d from %q, want %d from %q", c.Retries, c.Source("retries"), tc.retries, tc.retriesSrc)
			}
			if c.Server.BaseUrl != tc.url || c.Source("server.url") != tc.urlSrc {
				t.Errorf("server.url = %q from %q, want %q from %q", c.Server.BaseUrl, c.Source("server.url"), tc.url, tc.urlSrc)
			}
		})
	}
}

func TestLoadConfigEmptyToken(t *testing.T) {
	dir := isolate(t)
	file := writeFile(t, filepath.Join(dir, "c.yaml"), "server:\n  upload-token:\n", 0o600)
	c, _ := mustLoad(t, "--config", file)
	if c.Server.UploadToken != "" {
		t.Errorf("token = %q, want none for a null value", c.Server.UploadToken)
	}
}

func TestLoadConfigSecretFromEnv(t *testing.T) {
	isolate(t)
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", "1234567890")
	t.Setenv("LOCAL_REGISTRY_CA_FILE", "/ca.pem")
	c, _ := mustLoad(t)

	if c.Server.UploadToken != "1234567890" || c.Source("server.upload-token") != "env LOCAL_REGISTRY_UPLOAD_TOKEN" {
		t.Errorf("token %q from %q", c.Server.UploadToken, c.Source("server.upload-token"))
	}
	if c.Server.CaFile != "/ca.pem" || c.Source("server.ca-file") != "env LOCAL_REGISTRY_CA_FILE" {
		t.Errorf("ca-file %q from %q", c.Server.CaFile, c.Source("server.ca-file"))
	}
}

func TestLoadConfigPathFromEnv(t *testing.T) {
	dir := isolate(t)
	file := writeFile(t, filepath.Join(dir, "conf", "lr.yaml"), "retries: 4\n", 0o600)
	t.Setenv("CONF_DIR", filepath.Dir(file))
	t.Setenv("LOCAL_REGISTRY_CONFIG", "$CONF_DIR/lr.yaml")
	c, _ := mustLoad(t)

	if c.File() != file || c.Retries != 4 {
		t.Errorf("File() = %q, retries = %d, want %q and 4", c.File(), c.Retries, file)
	}
	if got := c.Source("config"); got != "env LOCAL_REGISTRY_CONFIG" {
		t.Errorf("Source(config) = %q", got)
	}
}

func TestLoadConfigSearch(t *testing.T) {
	t.Run("home before the working directory", func(t *testing.T) {
		dir := isolate(t)
		t.Chdir(t.TempDir())
		home := writeFile(t, filepath.Join(dir, ".lr", "config"), "retries: 1\n", 0o600)
		writeFile(t, "config.yaml", "retries: 2\n", 0o600)

		c, notes := mustLoad(t)
		if c.File() != home || c.Retries != 1 {
			t.Errorf("File() = %q, retries = %d, want %q and 1", c.File(), c.Retries, home)
		}
		if c.Source("config") != "default" || c.Source("retries") != "config file" {
			t.Errorf("sources: config %q, retries %q", c.Source("config"), c.Source("retries"))
		}
		if n, ok := findNote(notes, slog.LevelDebug, "config file found"); !ok || noteArg(n, "path") != home {
			t.Errorf("no 'config file found' note for %s in %v", home, notes)
		}
	})
	t.Run("a directory named config is skipped", func(t *testing.T) {
		dir := isolate(t)
		if err := os.Mkdir(filepath.Join(dir, "config"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "config.yml"), "retries: 6\n", 0o600)

		c, _ := mustLoad(t)
		if c.File() != "config.yml" || c.Retries != 6 {
			t.Errorf("File() = %q, retries = %d, want config.yml and 6", c.File(), c.Retries)
		}
	})
	t.Run("a directory with an unset variable is skipped", func(t *testing.T) {
		isolate(t)
		configSearchDirs = []string{"$LRCTL_TEST_UNSET", "."}
		writeFile(t, "config", "retries: 8\n", 0o600)

		c, _ := mustLoad(t)
		if c.Retries != 8 {
			t.Errorf("retries = %d, want 8", c.Retries)
		}
	})
}

func TestLoadConfigExpandsPaths(t *testing.T) {
	dir := isolate(t)
	t.Setenv("CERTS", "/certs")
	file := writeFile(t, filepath.Join(dir, "c.yaml"), "server:\n  ca-file: $CERTS/ca.pem\n", 0o600)

	c, _ := mustLoad(t, "--config", "~/c.yaml")
	if c.File() != file || c.Server.CaFile != "/certs/ca.pem" {
		t.Errorf("File() = %q, ca-file = %q", c.File(), c.Server.CaFile)
	}
	c, _ = mustLoad(t, "--server-ca-file", "~/ca.pem")
	if c.Server.CaFile != filepath.Join(dir, "ca.pem") {
		t.Errorf("ca-file = %q, want ~ expanded", c.Server.CaFile)
	}
}

func TestLoadConfigNormalizes(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		args   []string
		output string
		url    string
	}{
		{[]string{"-o", " JSON "}, "json", "http://localhost:8077"},
		{[]string{"--server-url", "https://r.example.com///"}, "text", "https://r.example.com"},
		{[]string{"--server-url="}, "text", ""},
	} {
		c, _ := mustLoad(t, tc.args...)
		if c.Output != tc.output || c.Server.BaseUrl != tc.url {
			t.Errorf("%q: output %q, url %q, want %q and %q", tc.args, c.Output, c.Server.BaseUrl, tc.output, tc.url)
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string // content of c.yaml, passed with --config when set
		args []string
		want []string
	}{
		{"unset variable in --config", "", []string{"--config", "$LRCTL_TEST_UNSET/c.yaml"}, []string{"config: environment variable $LRCTL_TEST_UNSET"}},
		{"missing file", "", []string{"--config", "nope.yaml"}, []string{"config file nope.yaml", "no such file"}},
		{"directory", "", []string{"--config", "."}, []string{"config file .: is a directory"}},
		{"invalid yaml", "server: [\n", nil, []string{"config file"}},
		{"unknown setting", "colour: true\n", nil, []string{`unknown setting "colour"`}},
		{"unknown nested setting", "server:\n  token: x\n", nil, []string{`unknown setting "server.token"`}},
		{"unquoted token", "server:\n  upload-token: 8888800000000000000000000001111111111188888888888888\n", nil,
			[]string{"server.upload-token: read as a float64", "quote it"}},
		{"bad int", "retries: abc\n", nil, []string{`retries: "abc" is not a whole number`}},
		{"bad bool", "verbose: maybe\n", nil, []string{"verbose:"}},
		{"bad string", "output:\n  a: 1\n", nil, []string{"output:"}},
		{"unset variable in a path", "server:\n  ca-file: $LRCTL_TEST_UNSET/ca.pem\n", nil, []string{"server.ca-file: environment variable"}},
		{"all bad values together", "retries: abc\ntimeout: x\n", nil, []string{"retries:", "timeout:"}},
		{"bad output", "", []string{"-o", "yaml"}, []string{`output "yaml"`}},
		{"quiet and verbose", "", []string{"-q", "-v"}, []string{"--quiet and --verbose"}},
		{"negative retries", "", []string{"--retries", "-1"}, []string{"retries cannot be negative"}},
		{"negative timeout", "", []string{"--timeout", "-1"}, []string{"timeout cannot be negative"}},
		{"negative exit code", "", []string{"--exit-code", "-1"}, []string{"exit-code -1 must be between 0 and 125"}},
		{"exit code of the shell", "exit-code: 126\n", nil, []string{"exit-code 126 must be between 0 and 125"}},
		{"url scheme", "", []string{"--server-url", "ftp://r.example.com"}, []string{"server.url ftp://r.example.com must be"}},
		{"url without host", "", []string{"--server-url", "https://"}, []string{"server.url"}},
		{"url with user", "", []string{"--server-url", "https://u:p@r.example.com"}, []string{"server.url"}},
		{"url with query", "", []string{"--server-url", "https://r.example.com?a=1"}, []string{"server.url"}},
		{"url with empty query", "", []string{"--server-url", "https://r.example.com?"}, []string{"server.url"}},
		{"url with fragment", "", []string{"--server-url", "https://r.example.com#x"}, []string{"server.url"}},
		{"url that does not parse", "", []string{"--server-url", "http://[::1"}, []string{"server.url"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolate(t)
			args := tc.args
			if tc.file != "" {
				args = append([]string{"--config", writeFile(t, filepath.Join(dir, "c.yaml"), tc.file, 0o600)}, args...)
			}
			_, _, err := load(t, args...)
			if err == nil {
				t.Fatal("LoadConfig() = nil, want an error")
			}
			if !IsUsageError(err) {
				t.Errorf("%v is not a usage error", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestLoadConfigTokenPermissions(t *testing.T) {
	const warning = "the config file holds the upload token but is readable by other users"
	for _, tc := range []struct {
		name    string
		content string
		mode    os.FileMode
		warn    bool
	}{
		{"token readable by others", "server:\n  upload-token: \"abc\"\n", 0o644, true},
		{"token readable by the group", "server:\n  upload-token: \"abc\"\n", 0o640, true},
		{"token private", "server:\n  upload-token: \"abc\"\n", 0o600, false},
		{"no token", "retries: 1\n", 0o644, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolate(t)
			file := writeFile(t, filepath.Join(dir, "c.yaml"), tc.content, tc.mode)
			_, notes := mustLoad(t, "--config", file)

			n, ok := findNote(notes, slog.LevelWarn, warning)
			if ok != tc.warn {
				t.Fatalf("warning = %v, want %v (notes %v)", ok, tc.warn, notes)
			}
			if ok && (noteArg(n, "fix") != "chmod 600 "+file || noteArg(n, "mode") != tc.mode.String()) {
				t.Errorf("warning args = %v", n.Args)
			}
		})
	}
}

func TestLoadConfigMissingFlag(t *testing.T) {
	isolate(t)
	fs := pflag.NewFlagSet("bare", pflag.ContinueOnError) // AddFlags not called
	var notes logging.Notes
	c, err := LoadConfig(fs, &notes)
	if err == nil || !strings.Contains(err.Error(), "--server-") {
		t.Fatalf("LoadConfig() = %v, want an error naming the missing flag", err)
	}
	if IsUsageError(err) {
		t.Error("a missing flag is a bug of the program, not a usage error")
	}
	if c == nil {
		t.Error("the Config is nil")
	}
}

// Two runs in a row must not share anything: this is why there is no global viper.
func TestLoadConfigRunsAreIsolated(t *testing.T) {
	dir := isolate(t)
	file := writeFile(t, filepath.Join(dir, "c.yaml"), "retries: 5\n", 0o600)

	first, _ := mustLoad(t, "--config", file)
	second, _ := mustLoad(t)

	if first.Retries != 5 {
		t.Errorf("first run: retries = %d, want 5", first.Retries)
	}
	if second.Retries != 3 || second.Source("retries") != "default" || second.File() != "" {
		t.Errorf("second run: retries = %d from %q, file %q: the first run leaked",
			second.Retries, second.Source("retries"), second.File())
	}
}

func TestOutputOptions(t *testing.T) {
	isolate(t)
	c, _ := mustLoad(t, "-o", "json", "-v", "--no-color")
	want := logging.Options{Format: logging.JSON, Verbose: true, NoColor: true}
	if got := c.OutputOptions(); got != want {
		t.Errorf("OutputOptions() = %+v, want %+v", got, want)
	}

	// After a failure, the error must still be printed in the format asked for.
	c, _, err := load(t, "-o", "json", "-q", "--config", "nope.yaml")
	if err == nil {
		t.Fatal("LoadConfig() = nil, want an error")
	}
	want = logging.Options{Format: logging.JSON, Quiet: true}
	if got := c.OutputOptions(); got != want {
		t.Errorf("OutputOptions() after a failure = %+v, want %+v", got, want)
	}
}

func TestWithAndFrom(t *testing.T) {
	isolate(t)
	c, _ := mustLoad(t)
	if got := From(With(context.Background(), c)); got != c {
		t.Errorf("From() = %p, want %p", got, c)
	}
	if got := From(context.Background()); got != nil {
		t.Errorf("From(empty) = %v, want nil", got)
	}
	var nilCtx context.Context
	if got := From(nilCtx); got != nil {
		t.Errorf("From(nil) = %v, want nil", got)
	}
}

func TestUsage(t *testing.T) {
	base := errors.New("bad flag")
	if Usage(nil) != nil {
		t.Error("Usage(nil) != nil")
	}
	u := Usage(base)
	if !IsUsageError(u) || !errors.Is(u, base) || u.Error() != "bad flag" {
		t.Errorf("Usage(%v) = %v", base, u)
	}
	if !IsUsageError(fmt.Errorf("lrctl: %w", u)) {
		t.Error("a wrapped usage error is not found")
	}
	if IsUsageError(base) || IsUsageError(nil) {
		t.Error("a plain error is a usage error")
	}
	if got := usagef("%s cannot be %d", "retries", -1); !IsUsageError(got) || got.Error() != "retries cannot be -1" {
		t.Errorf("usagef() = %v", got)
	}
}

func TestExpandPath(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	t.Setenv("LR_DIR", "/opt/lr")
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "", want: ""},
		{in: "/etc/ca.pem", want: "/etc/ca.pem"},
		{in: "$LR_DIR/ca.pem", want: "/opt/lr/ca.pem"},
		{in: "${LR_DIR}/ca.pem", want: "/opt/lr/ca.pem"},
		{in: "~", want: "/home/me"},
		{in: "~/ca.pem", want: "/home/me/ca.pem"},
		{in: "a~b", want: "a~b"},
		{in: "$NOPE_A/$NOPE_B", err: "environment variable $NOPE_A, $NOPE_B is not set"},
		{in: "~bob/ca.pem", err: "only ~ and ~/ are expanded, not ~user"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ExpandPath(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ExpandPath(%q) = %q, %v, want error %q", tc.in, got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ExpandPath(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
			}
		})
	}

	t.Run("no home directory", func(t *testing.T) {
		t.Setenv("HOME", "")
		if _, err := ExpandPath("~/x"); err == nil || !strings.Contains(err.Error(), "cannot expand ~") {
			t.Errorf("ExpandPath(~/x) without $HOME = %v", err)
		}
	})
}

func TestAddFlags(t *testing.T) {
	fs := pflag.NewFlagSet("lrctl", pflag.ContinueOnError)
	AddFlags(fs)

	for name, env := range map[string]string{
		"config":              "LOCAL_REGISTRY_CONFIG",
		"server-url":          "LOCAL_REGISTRY_URL",
		"server-upload-token": "LOCAL_REGISTRY_UPLOAD_TOKEN",
		"server-ca-file":      "LOCAL_REGISTRY_CA_FILE",
	} {
		if f := fs.Lookup(name); f == nil || !strings.HasSuffix(f.Usage, " - env["+env+"]") {
			t.Errorf("--%s does not name %s", name, env)
		}
	}
	for name, def := range map[string]string{"no-color": "false", "retries": "3", "timeout": "300"} {
		f := fs.Lookup(name)
		if f == nil || f.DefValue != def || strings.Contains(f.Usage, "env[") {
			t.Errorf("--%s: %+v, want default %s and no env", name, f, def)
		}
	}
	if fs.SortFlags {
		t.Error("flags are sorted: --help must keep their order")
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"url":          "LOCAL_REGISTRY_URL",
		"upload-token": "LOCAL_REGISTRY_UPLOAD_TOKEN",
		"no-color":     "LOCAL_REGISTRY_NO_COLOR",
		"a.b":          "LOCAL_REGISTRY_A_B",
	} {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileKeys(t *testing.T) {
	fs := newFlags(t)
	fs.Bool("help", false, "")
	keys := fileKeys(fs)
	for _, k := range append(slices.Clone(Keys), "server-url") {
		if !slices.Contains(keys, k) {
			t.Errorf("fileKeys() lacks %q", k)
		}
	}
	for _, k := range []string{"config", "help"} {
		if slices.Contains(keys, k) {
			t.Errorf("fileKeys() holds %q", k)
		}
	}
}

// The fields of httpclient.Client are not exported: each option is checked by what the client
// does against a test server.

// countingServer answers with the statuses in order (200 once they run out), and counts the
// requests by path.
func countingServer(t *testing.T, tls bool, statuses ...int) (*httptest.Server, func(path string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := hits[r.URL.Path]
		hits[r.URL.Path]++
		mu.Unlock()
		if n < len(statuses) {
			w.WriteHeader(statuses[n])
		}
		_, _ = w.Write([]byte(r.URL.Path))
	})
	srv := httptest.NewUnstartedServer(h)
	if tls {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[path]
	}
}

func newTestConfig(url string) *Config {
	return &Config{AppUntypedConfig: AppUntypedConfig{Server: ServerConf{BaseUrl: url}}}
}

func get(t *testing.T, cfg *Config, path string) (string, error) {
	t.Helper()
	resp, err := cfg.HttpClient().Get(context.Background(), path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestConfig_HttpClient(t *testing.T) {
	t.Run("same client", func(t *testing.T) {
		t.Parallel()
		cfg := newTestConfig("http://localhost:8077")
		if a, b := cfg.HttpClient(), cfg.HttpClient(); a != b {
			t.Error("HttpClient() returns a new client on each call: the cache is never shared")
		}
	})

	t.Run("base url", func(t *testing.T) {
		t.Parallel()
		srv, hits := countingServer(t, false)
		body, err := get(t, newTestConfig(srv.URL), "/registry/json-schema")
		if err != nil || body != "/registry/json-schema" || hits("/registry/json-schema") != 1 {
			t.Errorf("Get = %q, %v, want one request to %s/registry/json-schema", body, err, srv.URL)
		}
	})

	t.Run("other host", func(t *testing.T) {
		t.Parallel()
		srv, _ := countingServer(t, false)
		if _, err := get(t, newTestConfig(srv.URL), "http://example.org/x"); err == nil {
			t.Error("Get on another host succeeded: the client must stay on the server")
		}
	})

	t.Run("cache", func(t *testing.T) {
		t.Parallel()
		srv, hits := countingServer(t, false)
		cfg := newTestConfig(srv.URL)
		for range 2 {
			if _, err := get(t, cfg, "/a"); err != nil {
				t.Fatal(err)
			}
		}
		if n := hits("/a"); n != 1 {
			t.Errorf("server hit %d times for 2 GET, want 1 (cached)", n)
		}
	})

	t.Run("no retries", func(t *testing.T) {
		t.Parallel()
		srv, hits := countingServer(t, false, http.StatusServiceUnavailable)
		if _, err := get(t, newTestConfig(srv.URL), "/a"); err == nil {
			t.Error("Get succeeded on a 503 without retries")
		}
		if n := hits("/a"); n != 1 {
			t.Errorf("server hit %d times, want 1 with retries 0", n)
		}
	})

	t.Run("retries", func(t *testing.T) {
		t.Parallel()
		srv, hits := countingServer(t, false, http.StatusServiceUnavailable)
		cfg := newTestConfig(srv.URL)
		cfg.Retries = 1 // 1s of backoff
		if _, err := get(t, cfg, "/a"); err != nil {
			t.Errorf("Get after a 503 then a 200 = %v, want success", err)
		}
		if n := hits("/a"); n != 2 {
			t.Errorf("server hit %d times, want 2 with retries 1", n)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(func() { close(release); srv.Close() })
		cfg := newTestConfig(srv.URL)
		cfg.Timeout = 1

		start := time.Now()
		if _, err := get(t, cfg, "/slow"); err == nil {
			t.Error("Get succeeded on a server that never answers")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Get gave up after %v, want about the timeout of 1s", d)
		}
	})

	t.Run("ca file", func(t *testing.T) {
		t.Parallel()
		srv, _ := countingServer(t, true)
		ca := filepath.Join(t.TempDir(), "ca.pem")
		cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		if err := os.WriteFile(ca, cert, 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := get(t, newTestConfig(srv.URL), "/a"); err == nil {
			t.Error("Get succeeded without the CA of a self-signed server")
		}
		cfg := newTestConfig(srv.URL)
		cfg.Server.CaFile = ca
		if _, err := get(t, cfg, "/a"); err != nil {
			t.Errorf("Get with ca-file = %v, want success", err)
		}
	})
}
