package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nivekalara237/localrectl/config"
)

// The server has no endpoint to check the upload token: ping checks /healthz only, never sends
// the token, and only says whether one is configured (token_set). It must not claim the token
// is valid.

type seenRequest struct {
	Method, Path, Auth string
}

type fakeRegistry struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

// newFakeRegistry answers /healthz with health, and any other path with 404.
func newFakeRegistry(t *testing.T, health int) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		f.mu.Unlock()

		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(health)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRegistry) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func ping(t *testing.T, f *fakeRegistry, tok string, args ...string) result {
	t.Helper()
	if tok != "" {
		t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", tok)
	}
	return run(t, append([]string{"ping", "--server-url", f.URL}, args...)...)
}

// decodePing reads the JSON result by its keys, which are the contract with the scripts.
func decodePing(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	return got
}

// osStdout runs fn with os.Stdout redirected, and returns what was written on it: a command
// must write on cmd.OutOrStdout(), and a stray fmt.Println would break -o json.
func osStdout(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = wr
	defer func() { os.Stdout = saved }()

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(rd)
		done <- string(b)
	}()
	fn()
	_ = wr.Close()
	return <-done
}

func TestPingJSON(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	var r result
	if stray := osStdout(t, func() { r = ping(t, f, token, "-o", "json") }); stray != "" {
		t.Errorf("ping wrote on os.Stdout, outside of the Printer:\n%s", stray)
	}
	if r.err != nil {
		t.Fatalf("ping: %v\nstderr: %s", r.err, r.stderr)
	}

	got := decodePing(t, r.stdout)
	if got["url"] != f.URL+"/healthz" || got["status"] != float64(http.StatusOK) || got["token_set"] != true {
		t.Errorf("result = %v, want url %s/healthz, status 200, token_set true", got, f.URL)
	}
	if ms, ok := got["latency_ms"].(float64); !ok || ms < 0 {
		t.Errorf("latency_ms = %v, want a number >= 0", got["latency_ms"])
	}
	if _, ok := got["token_ok"]; ok {
		t.Error("token_ok is in the result: nothing on the server can tell that the token is valid")
	}
}

func TestPingText(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	r := ping(t, f, token)
	if r.err != nil {
		t.Fatalf("ping: %v\nstderr: %s", r.err, r.stderr)
	}

	if strings.Count(r.stdout, "\n") != 1 {
		t.Errorf("stdout = %q, want one line", r.stdout)
	}
	for _, w := range []string{f.URL + "/healthz", "200", "token set: true"} {
		if !strings.Contains(r.stdout, w) {
			t.Errorf("stdout %q does not contain %q", r.stdout, w)
		}
	}
	if strings.Contains(r.stdout, "token ok") {
		t.Errorf("stdout %q claims the token is valid", r.stdout)
	}
}

func TestPingWithoutToken(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	r := ping(t, f, "", "-o", "json")
	if r.err != nil {
		t.Fatalf("ping without a token: %v\nstderr: %s", r.err, r.stderr)
	}

	if got := decodePing(t, r.stdout); got["token_set"] != false || got["status"] != float64(http.StatusOK) {
		t.Errorf("result = %v, want status 200 and token_set false", got)
	}
}

func TestPingRequests(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
	// A trailing slash must not give //healthz.
	r := run(t, "ping", "--server-url", f.URL+"/", "-o", "json")
	if r.err != nil {
		t.Fatalf("ping: %v\nstderr: %s", r.err, r.stderr)
	}

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v, want one GET /healthz", reqs)
	}
	if reqs[0].Method != http.MethodGet || reqs[0].Path != "/healthz" {
		t.Errorf("request = %s %s, want GET /healthz", reqs[0].Method, reqs[0].Path)
	}
	if reqs[0].Auth != "" {
		t.Errorf("/healthz received Authorization %q: the token has no reason to leave the machine", reqs[0].Auth)
	}
}

func TestPingServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolate(t)
			f := newFakeRegistry(t, status)
			r := ping(t, f, token, "-o", "json")

			if r.err == nil {
				t.Fatalf("ping succeeded on a /healthz %d:\n%s", status, r.stdout)
			}
			if config.IsUsageError(r.err) {
				t.Errorf("%v is a usage error, want exit 1", r.err)
			}
			if !strings.Contains(r.err.Error(), strconv.Itoa(status)) {
				t.Errorf("error %q does not give the status %d", r.err, status)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing on an error", r.stdout)
			}
		})
	}
}

func TestPingUnreachable(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	f.Close() // nothing listens on the port any more
	r := ping(t, f, token, "-o", "json")

	if r.err == nil {
		t.Fatal("ping succeeded against a closed server")
	}
	if config.IsUsageError(r.err) {
		t.Errorf("%v is a usage error, want exit 1", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing on an error", r.stdout)
	}
}

func TestPingTimeout(t *testing.T) {
	isolate(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	start := time.Now()
	r := run(t, "ping", "--server-url", srv.URL, "--timeout", "1")
	if r.err == nil {
		t.Fatal("ping succeeded against a server that never answers")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("ping gave up after %v, want about --timeout 1s", d)
	}
}

// main prints the error: the command must not log it as well.
func TestPingErrorPrintedOnce(t *testing.T) {
	isolate(t)
	failing := newFakeRegistry(t, http.StatusInternalServerError)
	closed := newFakeRegistry(t, http.StatusOK)
	closed.Close()

	for name, f := range map[string]*fakeRegistry{"server error": failing, "unreachable": closed} {
		r := ping(t, f, token)
		if r.err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(r.stderr, "level=ERROR") {
			t.Errorf("%s: the error is logged, and main prints it again:\n%s", name, r.stderr)
		}
	}
}

func TestPingVerbose(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	r := ping(t, f, token, "-o", "json", "-v")
	if r.err != nil {
		t.Fatalf("ping: %v\nstderr: %s", r.err, r.stderr)
	}
	decodePing(t, r.stdout) // -v does not touch stdout

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(r.stderr), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		if rec["level"] == "DEBUG" && rec["msg"] == "request" && rec["url"] == f.URL+"/healthz" {
			found = true
			if rec["method"] != "GET" || rec["status"] != float64(200) {
				t.Errorf("request record = %v, want method GET and status 200", rec)
			}
		}
	}
	if !found {
		t.Errorf("no DEBUG 'request' record for /healthz with -v:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, token) {
		t.Error("the token is logged")
	}
}

func TestPingQuiet(t *testing.T) {
	isolate(t)
	f := newFakeRegistry(t, http.StatusOK)
	for _, args := range [][]string{{"-q"}, {}} {
		r := ping(t, f, token, args...)
		if r.err != nil {
			t.Fatalf("ping %v: %v", args, r.err)
		}
		if r.stderr != "" {
			t.Errorf("ping %v: stderr = %q, want nothing without -v", args, r.stderr)
		}
	}
}

func TestPingUsage(t *testing.T) {
	isolate(t)
	r := run(t, "ping", "extra")
	if r.err == nil || !config.IsUsageError(r.err) {
		t.Errorf("ping extra = %v, want a usage error", r.err)
	}
}

func TestPingWithoutRoot(t *testing.T) {
	cmd := newPingCommand()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Execute() = %v, want a 'not loaded' error", err)
	}
}
