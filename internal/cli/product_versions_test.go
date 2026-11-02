package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nivekalara237/localrectl/config"
	"github.com/spf13/cobra"
)

// productIndex is the index of bb-vcs-plugin: a deprecated version, a security update, the
// latest, and a prerelease without tag.
const productIndex = `{
  "displayName": "Bitbucket VCS plugin",
  "install": "lrctl install bb-vcs-plugin",
  "latest": "2.0.0",
  "versions": [
    {"version": "1.0.0", "tag": "v1.0.0", "publishedAt": "2026-01-10", "deprecated": true},
    {"version": "1.1.0", "tag": "v1.1.0", "publishedAt": "2026-03-02", "securityUpdates": true},
    {"version": "2.0.0", "tag": "v2.0.0", "publishedAt": "2026-06-20"},
    {"version": "2.1.0-rc.1", "publishedAt": "2026-09-01", "prerelease": true}
  ]
}`

type indexServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

// newIndexServer answers /<product>/index with status and body for every product.
func newIndexServer(t *testing.T, status int, body string) *indexServer {
	t.Helper()
	s := &indexServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		// The escaped path tells a/b (two segments) from a%2Fb (one).
		s.seen = append(s.seen, seenRequest{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")})
		s.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/index") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *indexServer) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

// versions runs without retries: a retried 5xx would wait 1s per attempt.
func versions(t *testing.T, s *indexServer, args ...string) result {
	t.Helper()
	return run(t, append([]string{"versions", "--server-url", s.URL, "--retries", "0"}, args...)...)
}

// rows returns the lines of the text table after its header.
func rows(t *testing.T, stdout string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "VERSION") {
		t.Fatalf("stdout has no VERSION header:\n%s", stdout)
	}
	return lines[1:]
}

func decodeVersions(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var got []map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, stdout)
	}
	return got
}

func versionNames(items []map[string]any) []string {
	var names []string
	for _, it := range items {
		names = append(names, it["version"].(string))
	}
	return names
}

func TestVersionsText(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	r := versions(t, s, "bb-vcs-plugin")
	if r.err != nil {
		t.Fatalf("versions: %v\nstderr: %s", r.err, r.stderr)
	}

	got := rows(t, r.stdout)
	if len(got) != 3 {
		t.Fatalf("rows = %q, want the 3 releases, without the prerelease", got)
	}
	for i, want := range [][]string{
		{"1.0.0 (Deprecated)", "v1.0.0", "-", "2026-01-10"},
		{"1.1.0", "v1.1.0", "True", "2026-03-02"},
		{"2.0.0 (Latest)", "v2.0.0", "-", "2026-06-20"},
	} {
		for _, cell := range want {
			if !strings.Contains(got[i], cell) {
				t.Errorf("row %d = %q, does not contain %q", i, got[i], cell)
			}
		}
	}
	if strings.Contains(r.stdout, "rc.1") {
		t.Errorf("the prerelease is listed without --prerelease:\n%s", r.stdout)
	}
}

func TestVersionsPrereleaseText(t *testing.T) {
	for _, flag := range []string{"--prerelease", "--pre"} {
		t.Run(flag, func(t *testing.T) {
			isolate(t)
			s := newIndexServer(t, http.StatusOK, productIndex)
			r := versions(t, s, "bb-vcs-plugin", flag)
			if r.err != nil {
				t.Fatalf("versions %s: %v", flag, r.err)
			}
			got := rows(t, r.stdout)
			if len(got) != 4 {
				t.Fatalf("rows = %q, want 4 with %s", got, flag)
			}
			last := strings.Fields(got[3])
			if len(last) != 5 || last[0] != "2.1.0-rc.1" || last[1] != "(Prerelease)" || last[2] != "-" {
				t.Errorf("prerelease row = %q, want 2.1.0-rc.1 (Prerelease) with tag -", got[3])
			}
		})
	}
}

// -o json lists the same versions as the text: a script must not see a prerelease that the
// table hides.
func TestVersionsJSON(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"1.0.0", "1.1.0", "2.0.0"}},
		{[]string{"--prerelease"}, []string{"1.0.0", "1.1.0", "2.0.0", "2.1.0-rc.1"}},
	} {
		t.Run(strings.Join(append([]string{"default"}, tc.args...), " "), func(t *testing.T) {
			isolate(t)
			s := newIndexServer(t, http.StatusOK, productIndex)
			var r result
			stray := osStdout(t, func() {
				r = versions(t, s, append([]string{"bb-vcs-plugin", "-o", "json"}, tc.args...)...)
			})
			if stray != "" {
				t.Errorf("versions wrote on os.Stdout, outside of the Printer:\n%s", stray)
			}
			if r.err != nil {
				t.Fatalf("versions: %v\nstderr: %s", r.err, r.stderr)
			}
			got := decodeVersions(t, r.stdout)
			if names := versionNames(got); strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Errorf("versions = %v, want %v", names, tc.want)
			}
		})
	}
}

// An index without versions is an empty list, not null: `jq '.[]'` fails on null.
func TestVersionsNone(t *testing.T) {
	for name, body := range map[string]string{
		"empty":   `{"latest": "", "versions": []}`,
		"missing": `{"latest": ""}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			s := newIndexServer(t, http.StatusOK, body)
			r := versions(t, s, "p", "-o", "json")
			if r.err != nil {
				t.Fatalf("versions: %v", r.err)
			}
			if got := strings.TrimSpace(r.stdout); got != "[]" {
				t.Errorf("stdout = %q, want []", got)
			}

			r = versions(t, s, "p")
			if r.err != nil || len(rows(t, r.stdout)) != 0 {
				t.Errorf("text = %q, %v, want the header alone", r.stdout, r.err)
			}
		})
	}
}

func TestVersionsRequest(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
	// A trailing slash must not give //bb-vcs-plugin/index.
	r := run(t, "versions", "--server-url", s.URL+"/", "bb-vcs-plugin")
	if r.err != nil {
		t.Fatalf("versions: %v", r.err)
	}

	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/bb-vcs-plugin/index" {
		t.Fatalf("requests = %+v, want one GET /bb-vcs-plugin/index", reqs)
	}
	if reqs[0].Auth != "" {
		t.Errorf("the index received Authorization %q: reading it needs no token", reqs[0].Auth)
	}
}

// The product name is one path segment: it must not change the path, the query or the host
// of the request.
func TestVersionsProductName(t *testing.T) {
	for _, name := range []string{"../registry/json-schema", "a/b", "x?y=1", "x#frag", "a b"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			s := newIndexServer(t, http.StatusOK, productIndex)
			r := versions(t, s, name)
			if config.IsUsageError(r.err) {
				if n := len(s.requests()); n != 0 {
					t.Errorf("%d request(s) for a refused name", n)
				}
				return
			}
			want := "/" + url.PathEscape(name) + "/index"
			reqs := s.requests()
			if len(reqs) != 1 || reqs[0].Path != want {
				t.Errorf("versions %q requested %+v, want only %q (or a usage error)", name, reqs, want)
			}
		})
	}
}

// An absolute URL as the name must not send the request to another server.
func TestVersionsProductNameURL(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	other := newIndexServer(t, http.StatusOK, productIndex)
	r := versions(t, s, other.URL+"/x")
	if n := len(other.requests()); n != 0 {
		t.Errorf("versions %s sent %d request(s) to it (err %v)", other.URL+"/x", n, r.err)
	}
}

func TestVersionsEmptyName(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	r := versions(t, s, "")
	if !config.IsUsageError(r.err) {
		t.Errorf("versions '' = %v, want a usage error", r.err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d request(s) for an empty name", n)
	}
}

func TestVersionsNotFound(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusNotFound, `{"error":"not found"}`)
	r := versions(t, s, "nope", "-o", "json")
	if r.err == nil || !strings.Contains(r.err.Error(), "nope not found") {
		t.Fatalf("versions nope = %v, want 'nope not found'", r.err)
	}
	if config.IsUsageError(r.err) {
		t.Errorf("%v is a usage error, want the exit-code setting", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

func TestVersionsServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolate(t)
			s := newIndexServer(t, status, `{"error":"nope"}`)
			r := versions(t, s, "p", "-o", "json")
			if r.err == nil {
				t.Fatalf("versions succeeded on a %d:\n%s", status, r.stdout)
			}
			if config.IsUsageError(r.err) {
				t.Errorf("%v is a usage error", r.err)
			}
			if !strings.Contains(r.err.Error(), strconv.Itoa(status)) {
				t.Errorf("error %q does not give the status %d", r.err, status)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
		})
	}
}

func TestVersionsBadBody(t *testing.T) {
	for name, body := range map[string]string{"not json": "<html>", "null": "null", "array": "[1, 2]"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			s := newIndexServer(t, http.StatusOK, body)
			r := versions(t, s, "p", "-o", "json")
			if r.err == nil {
				t.Fatalf("versions succeeded on the body %q:\n%s", body, r.stdout)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
		})
	}
}

func TestVersionsUnreachable(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	s.Close()
	r := versions(t, s, "p")
	if r.err == nil || !strings.Contains(r.err.Error(), "connection refused") {
		t.Errorf("versions against a closed server = %v, want the cause", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

// main prints the error: the command must not log it as well.
func TestVersionsErrorPrintedOnce(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusInternalServerError, "boom")
	r := versions(t, s, "p")
	if r.err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(r.stderr, "level=ERROR") {
		t.Errorf("the error is logged, and main prints it again:\n%s", r.stderr)
	}
}

// A response from the cache has no Request: a second search must not dereference it.
func TestVersionsFromCache(t *testing.T) {
	s := newIndexServer(t, http.StatusOK, productIndex)
	cfg := &config.Config{AppUntypedConfig: config.AppUntypedConfig{Server: config.ServerConf{BaseUrl: s.URL}}}
	v := newProductVersionsCmd(cfg, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	defer func() {
		if p := recover(); p != nil {
			t.Errorf("the search from the cache panics: %v", p)
		}
	}()
	for i := range 2 {
		res, err := v.runProductVersionsSearch(cmd, "bb-vcs-plugin")
		if err != nil || res.index == nil || res.index.Latest != "2.0.0" {
			t.Fatalf("search #%d = %+v, %v", i+1, res, err)
		}
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1 (the second from the cache)", n)
	}
}

func TestVersionsVerbose(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
	r := versions(t, s, "bb-vcs-plugin", "-o", "json", "-v")
	if r.err != nil {
		t.Fatal(r.err)
	}
	decodeVersions(t, r.stdout) // -v does not touch stdout

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(r.stderr), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		if rec["level"] == "DEBUG" && rec["msg"] == "request" && rec["url"] == s.URL+"/bb-vcs-plugin/index" {
			found = true
			if rec["cmd"] != "versions" || rec["method"] != "GET" || rec["status"] != float64(200) {
				t.Errorf("request record = %v, want cmd versions, method GET and status 200", rec)
			}
		}
	}
	if !found {
		t.Errorf("no DEBUG 'request' record for the index with -v:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, token) {
		t.Error("the token is logged")
	}
}

func TestVersionsQuiet(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	for _, args := range [][]string{{"-q"}, {}} {
		r := versions(t, s, append([]string{"p"}, args...)...)
		if r.err != nil || r.stderr != "" {
			t.Errorf("versions %v: err %v, stderr %q, want nothing on stderr", args, r.err, r.stderr)
		}
	}
}

func TestVersionsAlias(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	r := run(t, "product-versions", "--server-url", s.URL, "bb-vcs-plugin")
	if r.err != nil || r.cmd.Name() != "versions" {
		t.Errorf("product-versions = %v (command %q), want an alias of versions", r.err, r.cmd.Name())
	}
}

func TestVersionsUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"versions"}, {"versions", "a", "b"}, {"versions", "a", "--nope"}} {
		if r := run(t, args...); !config.IsUsageError(r.err) {
			t.Errorf("%v = %v, want a usage error", args, r.err)
		}
	}
}

func TestVersionsWithoutRoot(t *testing.T) {
	cmd := newVersionsCommand()
	cmd.SetArgs([]string{"p"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Execute() = %v, want a 'not loaded' error", err)
	}
}

func TestVersionsOutputFails(t *testing.T) {
	isolate(t)
	s := newIndexServer(t, http.StatusOK, productIndex)
	for _, o := range []string{"text", "json"} {
		root := NewRootCommand()
		root.SetArgs([]string{"versions", "--server-url", s.URL, "p", "-o", o})
		root.SetOut(failWriter{})
		root.SetErr(io.Discard)
		if _, err := root.ExecuteC(); !errors.Is(err, errWrite) {
			t.Errorf("-o %s on a failing stdout = %v, want %v", o, err, errWrite)
		}
	}
}

func TestVersionsDisplayWithoutIndex(t *testing.T) {
	v := &productVersionsCmd{}
	if err := v.displayResult(&versionsResult{}, nil, false); err == nil {
		t.Error("displayResult without an index = nil, want an error")
	}
}
