package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nivekalara237/localrectl/config"
)

const productSchema = `{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`

// schemaServer answers productSchemaPath with status and body, and counts the requests.
func schemaServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != productSchemaPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func card(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// validate runs without retries: a retried 5xx would wait 1s per attempt.
func validate(t *testing.T, url string, args ...string) result {
	t.Helper()
	return run(t, append([]string{"validate", "--server-url", url, "--retries", "0"}, args...)...)
}

func decodeValidate(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	return got
}

func productStatus(t *testing.T, got map[string]any) any {
	t.Helper()
	product, ok := got["product"].(map[string]any)
	if !ok {
		t.Fatalf("no product in %v", got)
	}
	return product["status"]
}

func TestValidateValid(t *testing.T) {
	for _, o := range []string{"text", "json"} {
		t.Run(o, func(t *testing.T) {
			isolate(t)
			srv, _ := schemaServer(t, http.StatusOK, productSchema)
			r := validate(t, srv.URL, card(t, "name: gitlab-plugin\n"), "-o", o)
			if r.err != nil {
				t.Fatalf("validate a valid card: %v\nstdout: %s", r.err, r.stdout)
			}
			if o == "text" {
				if !strings.Contains(r.stdout, "STATUS: OK") {
					t.Errorf("stdout = %q, want STATUS: OK", r.stdout)
				}
				return
			}
			got := decodeValidate(t, r.stdout)
			if productStatus(t, got) != "OK" {
				t.Errorf("result = %v, want product OK", got)
			}
			if _, ok := got["artifacts"]; ok {
				t.Errorf("result = %v, want no artifacts without --artifacts", got)
			}
		})
	}
}

// An invalid card fails the command in JSON as in text: a CI reads the exit code.
func TestValidateInvalid(t *testing.T) {
	for _, o := range []string{"text", "json"} {
		t.Run(o, func(t *testing.T) {
			isolate(t)
			srv, _ := schemaServer(t, http.StatusOK, productSchema)
			r := validate(t, srv.URL, card(t, "other: 1\n"), "-o", o)
			if !errors.Is(r.err, errInvalidProductCard) {
				t.Fatalf("validate an invalid card = %v, want %v", r.err, errInvalidProductCard)
			}
			if config.IsUsageError(r.err) {
				t.Errorf("%v is a usage error, want exit 1", r.err)
			}
			if o == "text" {
				if !strings.Contains(r.stdout, "STATUS: KO") || !strings.Contains(r.stdout, "name is required") {
					t.Errorf("stdout = %q, want STATUS: KO and the mistake", r.stdout)
				}
				return
			}
			got := decodeValidate(t, r.stdout)
			if productStatus(t, got) != "KO" {
				t.Errorf("result = %v, want product KO", got)
			}
			if strings.Contains(r.stdout, `"expected"`) {
				t.Errorf("stdout holds empty fields:\n%s", r.stdout)
			}
		})
	}
}

// The body of an error page is not the schema: validating against it accepts any card.
func TestValidateServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolate(t)
			srv, _ := schemaServer(t, status, `{"error":"nope"}`)
			r := validate(t, srv.URL, card(t, "other: 1\n"), "-o", "json")
			if r.err == nil {
				t.Fatalf("validate succeeded on a schema served with %d:\n%s", status, r.stdout)
			}
			if errors.Is(r.err, errInvalidProductCard) || config.IsUsageError(r.err) {
				t.Errorf("error = %v, want a failure to fetch the schema", r.err)
			}
			if !strings.Contains(r.err.Error(), strconv.Itoa(status)) {
				t.Errorf("error %q does not give the status %d", r.err, status)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing when the card cannot be checked", r.stdout)
			}
		})
	}
}

func TestValidateUnreachable(t *testing.T) {
	isolate(t)
	srv, _ := schemaServer(t, http.StatusOK, productSchema)
	srv.Close()
	r := validate(t, srv.URL, card(t, "name: x\n"))
	if r.err == nil || errors.Is(r.err, errInvalidProductCard) {
		t.Fatalf("validate against a closed server = %v, want a network error", r.err)
	}
	if !strings.Contains(r.err.Error(), "connection refused") {
		t.Errorf("error %q hides the cause", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

// A file that cannot be read is reported without a request to the server.
func TestValidateBadFile(t *testing.T) {
	for name, path := range map[string]string{
		"missing":  filepath.Join(os.TempDir(), "nope", "registry.yaml"),
		"not yaml": "registry.json",
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			srv, hits := schemaServer(t, http.StatusOK, productSchema)
			r := validate(t, srv.URL, path, "-o", "json")
			if !errors.Is(r.err, errInvalidProductCard) {
				t.Errorf("validate %s = %v, want %v", path, r.err, errInvalidProductCard)
			}
			if productStatus(t, decodeValidate(t, r.stdout)) != "KO" {
				t.Errorf("stdout = %s, want product KO", r.stdout)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("%d request(s) to the server for a file that cannot be read", n)
			}
		})
	}
}

func TestValidateBrokenYAML(t *testing.T) {
	isolate(t)
	srv, hits := schemaServer(t, http.StatusOK, productSchema)
	r := validate(t, srv.URL, card(t, "name: [unclosed\n"))
	if !errors.Is(r.err, errInvalidProductCard) || !strings.Contains(r.stdout, "parse yaml") {
		t.Errorf("validate a broken YAML = %v\n%s", r.err, r.stdout)
	}
	if hits.Load() != 0 {
		t.Error("request to the server for a broken file")
	}
}

func TestValidateBadSchema(t *testing.T) {
	isolate(t)
	srv, _ := schemaServer(t, http.StatusOK, "not json")
	r := validate(t, srv.URL, card(t, "name: x\n"))
	if !errors.Is(r.err, errInvalidProductCard) || !strings.Contains(r.stdout, "unable to valide schema") {
		t.Errorf("validate against a broken schema = %v\n%s", r.err, r.stdout)
	}
}

func TestValidateArtifacts(t *testing.T) {
	cases := map[string]struct {
		content string // "" for a missing file
		help    bool
	}{
		"missing":  {},
		"not json": {content: "not json", help: true},
		"invalid":  {content: `"a string"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			srv, _ := schemaServer(t, http.StatusOK, productSchema)
			path := filepath.Join(t.TempDir(), "artifacts.json")
			if c.content != "" {
				if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := validate(t, srv.URL, card(t, "name: x\n"), "--artifacts", path, "-o", "json")
			if !errors.Is(r.err, errInvalidArtifacts) || errors.Is(r.err, errInvalidProductCard) {
				t.Fatalf("validate = %v, want only %v", r.err, errInvalidArtifacts)
			}
			got := decodeValidate(t, r.stdout)
			artifacts, ok := got["artifacts"].(map[string]any)
			if !ok || artifacts["status"] != "KO" || productStatus(t, got) != "OK" {
				t.Errorf("result = %v, want product OK and artifacts KO", got)
			}
			if h := strings.Contains(r.stdout, artifactsSchemaHelp); h != c.help {
				t.Errorf("help link printed = %v, want %v:\n%s", h, c.help, r.stdout)
			}
		})
	}
}

func TestValidateBothInvalidText(t *testing.T) {
	isolate(t)
	srv, _ := schemaServer(t, http.StatusOK, productSchema)
	r := validate(t, srv.URL, card(t, "other: 1\n"), "--artifacts", filepath.Join(t.TempDir(), "none.json"))
	if !errors.Is(r.err, errInvalidProductCard) || !errors.Is(r.err, errInvalidArtifacts) {
		t.Errorf("error = %v, want both files invalid", r.err)
	}
	for _, w := range []string{"[ PRODUCT CARD ]", "[ ARTIFACT FILE ]"} {
		if !strings.Contains(r.stdout, w) {
			t.Errorf("stdout does not contain %q:\n%s", w, r.stdout)
		}
	}
}

// A response from the cache has no Request: the second fetch must not dereference it.
func TestFetchProductSchemaFromCache(t *testing.T) {
	srv, hits := schemaServer(t, http.StatusOK, productSchema)
	cfg := &config.Config{AppUntypedConfig: config.AppUntypedConfig{Server: config.ServerConf{BaseUrl: srv.URL}}}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	for i := range 2 {
		body, err := fetchProductSchemaSpec(context.Background(), cfg, log)
		if err != nil || string(body) != productSchema {
			t.Fatalf("fetch #%d = %q, %v", i+1, body, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests, want 1 (the second from the cache)", n)
	}
	if !strings.Contains(logs.String(), "url="+srv.URL+productSchemaPath) {
		t.Errorf("logs do not give the url:\n%s", logs.String())
	}
}

func TestValidateVerbose(t *testing.T) {
	isolate(t)
	srv, _ := schemaServer(t, http.StatusOK, productSchema)
	r := validate(t, srv.URL, card(t, "name: x\n"), "-v")
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, w := range []string{"cmd=validate", "step=product", "url=" + srv.URL + productSchemaPath, "status=200"} {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("stderr does not contain %q:\n%s", w, r.stderr)
		}
	}
}

func TestValidateUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"validate"}, {"validate", "a.yaml", "b.yaml"}} {
		if r := run(t, args...); !config.IsUsageError(r.err) {
			t.Errorf("%v = %v, want a usage error", args, r.err)
		}
	}
}

func TestValidateWithoutRoot(t *testing.T) {
	cmd := newValidateProductCommand()
	cmd.SetArgs([]string{"registry.yaml"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Execute() = %v, want a 'not loaded' error", err)
	}
}

func TestValidateOutputFails(t *testing.T) {
	isolate(t)
	srv, _ := schemaServer(t, http.StatusOK, productSchema)
	root := NewRootCommand()
	root.SetArgs([]string{"validate", "--server-url", srv.URL, card(t, "name: x\n")})
	root.SetOut(failWriter{})
	root.SetErr(io.Discard)
	if _, err := root.ExecuteC(); !errors.Is(err, errWrite) {
		t.Errorf("validate on a failing stdout = %v, want %v", err, errWrite)
	}
}
