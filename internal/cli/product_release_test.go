package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nivekalara237/localrectl/config"
)

const releaseJSON = `{"version":"v1.6.23","publishedAt":"2026-06-20","prerelease":false,` +
	`"assets":[{"name":"plugin.jar","sha256":"abc"}]}`

type releaseServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

// newReleaseServer answers /<product>/releases/<version> with status and body.
func newReleaseServer(t *testing.T, status int, body string) *releaseServer {
	t.Helper()
	s := &releaseServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, seenRequest{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")})
		s.mu.Unlock()
		if !strings.Contains(r.URL.Path, "/releases/") {
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

func (s *releaseServer) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

// release runs without retries: a retried 5xx would wait 1s per attempt.
func release(t *testing.T, s *releaseServer, args ...string) result {
	t.Helper()
	return run(t, append([]string{"release", "--server-url", s.URL, "--retries", "0"}, args...)...)
}

// sameJSON tells whether got is the JSON document want, whatever the indentation.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Errorf("stdout is not JSON: %v\n%s", err, got)
		return false
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(g, w)
}

// The release.json of the server is printed as is: in text too, indented.
func TestReleaseOutput(t *testing.T) {
	for _, o := range []string{"text", "json"} {
		t.Run(o, func(t *testing.T) {
			isolate(t)
			s := newReleaseServer(t, http.StatusOK, releaseJSON)
			var r result
			stray := osStdout(t, func() { r = release(t, s, "gitlab-vcs-plugin", "v1.6.23", "-o", o) })
			if stray != "" {
				t.Errorf("release wrote on os.Stdout, outside of the Printer:\n%s", stray)
			}
			if r.err != nil {
				t.Fatalf("release: %v\nstderr: %s", r.err, r.stderr)
			}
			if !sameJSON(t, r.stdout, releaseJSON) {
				t.Errorf("stdout = %s, want the release.json of the server", r.stdout)
			}
			if !strings.Contains(r.stdout, "\n  ") {
				t.Errorf("stdout is not indented:\n%s", r.stdout)
			}
		})
	}
}

func TestReleaseRequest(t *testing.T) {
	for _, tc := range []struct {
		version, path string
	}{
		{"v1.6.23", "/gitlab-vcs-plugin/releases/v1.6.23"},
		{"latest", "/gitlab-vcs-plugin/releases/latest"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			isolate(t)
			s := newReleaseServer(t, http.StatusOK, releaseJSON)
			t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
			// A trailing slash must not give //gitlab-vcs-plugin/….
			r := run(t, "release", "--server-url", s.URL+"/", "gitlab-vcs-plugin", tc.version)
			if r.err != nil {
				t.Fatalf("release: %v", r.err)
			}
			reqs := s.requests()
			if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != tc.path {
				t.Fatalf("requests = %+v, want one GET %s", reqs, tc.path)
			}
			if reqs[0].Auth != "" {
				t.Errorf("the release received Authorization %q: reading it needs no token", reqs[0].Auth)
			}
		})
	}
}

// The product and the version are one path segment each: they must not change the path of
// the request.
func TestReleaseInvalidArgs(t *testing.T) {
	for _, args := range [][]string{
		{"", "v1"}, {".", "v1"}, {"..", "v1"}, {"a/b", "v1"}, {`a\b`, "v1"},
		{"p", ""}, {"p", "."}, {"p", ".."}, {"p", "../../registry/json-schema"}, {"p", "v1/x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			s := newReleaseServer(t, http.StatusOK, releaseJSON)
			r := release(t, s, args...)
			if !config.IsUsageError(r.err) {
				t.Errorf("release %q = %v, want a usage error", args, r.err)
			}
			if reqs := s.requests(); len(reqs) != 0 {
				t.Errorf("requests %+v for invalid arguments", reqs)
			}
		})
	}
}

func TestReleaseNotFound(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusNotFound, `{"error":"not found"}`)
	r := release(t, s, "nope", "v9", "-o", "json")
	if r.err == nil || !strings.Contains(r.err.Error(), "not found") ||
		!strings.Contains(r.err.Error(), "nope") || !strings.Contains(r.err.Error(), "v9") {
		t.Fatalf("release nope v9 = %v, want a 'not found' naming nope and v9", r.err)
	}
	if config.IsUsageError(r.err) {
		t.Errorf("%v is a usage error, want the exit-code setting", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

func TestReleaseServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolate(t)
			s := newReleaseServer(t, status, `{"error":"nope"}`)
			r := release(t, s, "p", "v1", "-o", "json")
			if r.err == nil {
				t.Fatalf("release succeeded on a %d:\n%s", status, r.stdout)
			}
			if config.IsUsageError(r.err) {
				t.Errorf("%v is a usage error", r.err)
			}
			if msg := r.err.Error(); !strings.Contains(msg, strconv.Itoa(status)) || !strings.Contains(msg, "product release") {
				t.Errorf("error %q does not give the status %d of the release", msg, status)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
		})
	}
}

// A body that is not JSON is an error in text as in JSON, not a blank line.
func TestReleaseBadBody(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "html": "<html>", "truncated": `{"version":`} {
		for _, o := range []string{"text", "json"} {
			t.Run(name+" "+o, func(t *testing.T) {
				isolate(t)
				s := newReleaseServer(t, http.StatusOK, body)
				r := release(t, s, "p", "v1", "-o", o)
				if r.err == nil {
					t.Errorf("release succeeded on the body %q:\n%q", body, r.stdout)
				}
				if strings.TrimSpace(r.stdout) != "" {
					t.Errorf("stdout = %q, want nothing", r.stdout)
				}
			})
		}
	}
}

func TestReleaseUnreachable(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusOK, releaseJSON)
	s.Close()
	r := release(t, s, "p", "v1")
	if r.err == nil || !strings.Contains(r.err.Error(), "connection refused") {
		t.Errorf("release against a closed server = %v, want the cause", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

// main prints the error: the command must not log it as well.
func TestReleaseErrorPrintedOnce(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusInternalServerError, "boom")
	r := release(t, s, "p", "v1")
	if r.err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(r.stderr, "level=ERROR") {
		t.Errorf("the error is logged, and main prints it again:\n%s", r.stderr)
	}
}

func TestReleaseVerbose(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusOK, releaseJSON)
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
	r := release(t, s, "gitlab-vcs-plugin", "v1.6.23", "-o", "json", "-v")
	if r.err != nil {
		t.Fatal(r.err)
	}
	sameJSON(t, r.stdout, releaseJSON) // -v does not touch stdout

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(r.stderr), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		if rec["level"] == "DEBUG" && rec["msg"] == "request" && rec["url"] == s.URL+"/gitlab-vcs-plugin/releases/v1.6.23" {
			found = true
			if rec["cmd"] != "release" || rec["method"] != "GET" || rec["status"] != float64(200) {
				t.Errorf("request record = %v, want cmd release, method GET and status 200", rec)
			}
		}
	}
	if !found {
		t.Errorf("no DEBUG 'request' record for the release with -v:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, token) {
		t.Error("the token is logged")
	}
}

func TestReleaseQuiet(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusOK, releaseJSON)
	for _, args := range [][]string{{"-q"}, {}} {
		r := release(t, s, append([]string{"p", "v1"}, args...)...)
		if r.err != nil || r.stderr != "" {
			t.Errorf("release %v: err %v, stderr %q, want nothing on stderr", args, r.err, r.stderr)
		}
	}
}

func TestReleaseUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"release"}, {"release", "p"}, {"release", "p", "v1", "x"}, {"release", "p", "v1", "--nope"}} {
		if r := run(t, args...); !config.IsUsageError(r.err) {
			t.Errorf("%v = %v, want a usage error", args, r.err)
		}
	}
}

// The example of the help must run as written.
func TestReleaseExample(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusOK, releaseJSON)
	cmd := newProductReleaseCommand()
	for line := range strings.SplitSeq(cmd.Example, "\n") {
		fields := strings.Fields(strings.ReplaceAll(strings.TrimSpace(line), "'", ""))
		if len(fields) == 0 || fields[0] == "#" {
			continue
		}
		if fields[0] != "lrctl" {
			t.Fatalf("example %q does not start with lrctl", line)
		}
		if i := slices.Index(fields, "|"); i >= 0 {
			fields = fields[:i]
		}
		if i := slices.Index(fields, "--server-url"); i >= 0 {
			fields = slices.Delete(fields, i, i+2)
		}
		r := release(t, s, fields[2:]...)
		if r.err != nil {
			t.Errorf("example %q = %v", line, r.err)
		}
	}
}

func TestReleaseUseLine(t *testing.T) {
	if use := newProductReleaseCommand().Use; strings.Contains(use, `\`) {
		t.Errorf("Use = %q: the help prints the backslash", use)
	}
}

func TestReleaseWithoutRoot(t *testing.T) {
	cmd := newProductReleaseCommand()
	cmd.SetArgs([]string{"p", "v1"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Execute() = %v, want a 'not loaded' error", err)
	}
}

func TestReleaseOutputFails(t *testing.T) {
	isolate(t)
	s := newReleaseServer(t, http.StatusOK, releaseJSON)
	for _, o := range []string{"text", "json"} {
		root := NewRootCommand()
		root.SetArgs([]string{"release", "--server-url", s.URL, "p", "v1", "-o", o})
		root.SetOut(failWriter{})
		root.SetErr(io.Discard)
		if _, err := root.ExecuteC(); !errors.Is(err, errWrite) {
			t.Errorf("-o %s on a failing stdout = %v, want %v", o, err, errWrite)
		}
	}
}
