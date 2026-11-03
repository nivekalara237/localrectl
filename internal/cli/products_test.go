package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nivekalara237/localrectl/config"
)

const (
	gitlabProduct = `{"product":"gitlab-vcs-plugin","displayName":"GitLab VCS plugin","description":"d",` +
		`"latest":"v1.6.23","updatedAt":"2026-06-20","keywords":["vcs"]}`
	bbProduct    = `{"product":"bb-vcs-plugin","displayName":"","latest":null,"updatedAt":"2026-03-02"}`
	sonarProduct = `{"product":"sonar-plugin","displayName":"Sonar plugin","latest":"2.0.0","updatedAt":"2026-09-01"}`
)

func productsPageJSON(next string, products ...string) string {
	b := `{"items":[` + strings.Join(products, ",") + `]`
	if next != "" {
		b += `,"next":"` + next + `"`
	}
	return b + "}"
}

type productsServer struct {
	*httptest.Server
	mu      sync.Mutex
	queries []url.Values
}

// newProductsServer answers /products with pages[cursor], the cursor "" being the first page,
// and with status when it is not 200.
func newProductsServer(t *testing.T, status int, pages map[string]string) *productsServer {
	t.Helper()
	s := &productsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.Query())
		s.mu.Unlock()
		if r.URL.Path != "/products" {
			http.NotFound(w, r)
			return
		}
		body, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			http.Error(w, "unknown cursor", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func onePage(products ...string) map[string]string {
	return map[string]string{"": productsPageJSON("", products...)}
}

func (s *productsServer) requests() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.queries)
}

// products runs without retries: a retried 5xx would wait 1s per attempt.
func products(t *testing.T, s *productsServer, args ...string) result {
	t.Helper()
	return run(t, append([]string{"products", "--server-url", s.URL, "--retries", "0"}, args...)...)
}

type productsJSON struct {
	Products []map[string]any `json:"products"`
	Next     *string          `json:"next"`
}

func decodeProducts(t *testing.T, stdout string) productsJSON {
	t.Helper()
	var got productsJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
	}
	if got.Products == nil {
		t.Fatalf("no products array in %s", stdout)
	}
	return got
}

func productNames(got productsJSON) []string {
	var names []string
	for _, p := range got.Products {
		names = append(names, p["product"].(string))
	}
	return names
}

func TestProductsText(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct, bbProduct))
	r := products(t, s)
	if r.err != nil {
		t.Fatalf("products: %v\nstderr: %s", r.err, r.stderr)
	}

	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "PRODUCT") {
		t.Fatalf("stdout = %q, want a header and 2 rows", r.stdout)
	}
	for i, want := range [][]string{
		{"gitlab-vcs-plugin", "v1.6.23", "2026-06-20", "GitLab", "VCS", "plugin"},
		{"bb-vcs-plugin", "-", "2026-03-02", "-"},
	} {
		if got := strings.Fields(lines[i+1]); !reflect.DeepEqual(got, want) {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want nothing without a next page", r.stderr)
	}
}

// Each product is printed as the server returns it: a field lrctl does not know is kept.
func TestProductsJSON(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct, bbProduct))
	var r result
	if stray := osStdout(t, func() { r = products(t, s, "-o", "json") }); stray != "" {
		t.Errorf("products wrote on os.Stdout, outside of the Printer:\n%s", stray)
	}
	if r.err != nil {
		t.Fatalf("products: %v", r.err)
	}
	got := decodeProducts(t, r.stdout)
	if names := productNames(got); !slices.Equal(names, []string{"gitlab-vcs-plugin", "bb-vcs-plugin"}) {
		t.Errorf("products = %v", names)
	}
	if kw, _ := got.Products[0]["keywords"].([]any); len(kw) != 1 || kw[0] != "vcs" {
		t.Errorf("keywords = %v, want the field of the server kept", got.Products[0]["keywords"])
	}
	if got.Products[1]["latest"] != nil {
		t.Errorf("latest = %v, want null as the server gives it", got.Products[1]["latest"])
	}
	if got.Next != nil {
		t.Errorf("next = %q, want no next on the last page", *got.Next)
	}
}

func TestProductsEmpty(t *testing.T) {
	for _, body := range []string{`{"items":[]}`, `[]`} {
		t.Run(body, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, map[string]string{"": body})
			r := products(t, s, "-o", "json")
			if r.err != nil {
				t.Fatalf("products: %v", r.err)
			}
			if got := decodeProducts(t, r.stdout); len(got.Products) != 0 {
				t.Errorf("products = %v, want []", got.Products)
			}
			r = products(t, s)
			if r.err != nil || strings.Count(r.stdout, "\n") != 1 {
				t.Errorf("text = %q, %v, want the header alone", r.stdout, r.err)
			}
		})
	}
}

// A server that answers a bare array (no pages) is a single page.
func TestProductsBareArray(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, map[string]string{"": "[" + gitlabProduct + "," + sonarProduct + "]"})
	r := products(t, s, "-o", "json", "--all")
	if r.err != nil {
		t.Fatalf("products: %v", r.err)
	}
	if names := productNames(decodeProducts(t, r.stdout)); !slices.Equal(names, []string{"gitlab-vcs-plugin", "sonar-plugin"}) {
		t.Errorf("products = %v", names)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestProductsQuery(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want url.Values
	}{
		{"none", nil, url.Values{}},
		{"all criteria", []string{
			"--query", "git lab", "-k", "vcs", "--keyword", "plugin", "-l", "team=devtools", "--label", "tier=1",
			"--owner", "dev@example.com", "--license", "MIT", "--deprecated", "--prerelease=false",
			"--updated-since", "2026-01-31", "--sort", "updatedAt", "--limit", "20", "-c", "abc",
		}, url.Values{
			"q": {"git lab"}, "keyword": {"vcs", "plugin"}, "label": {"team=devtools", "tier=1"},
			"owner": {"dev@example.com"}, "license": {"MIT"}, "deprecated": {"true"}, "prerelease": {"false"},
			"updatedSince": {"2026-01-31"}, "sort": {"updatedAt"}, "limit": {"20"}, "cursor": {"abc"},
		}},
		{"short query", []string{"-Q", "x"}, url.Values{"q": {"x"}}},
		{"rfc 3339", []string{"--updated-since", "2026-01-31T10:00:00Z"}, url.Values{"updatedSince": {"2026-01-31T10:00:00Z"}}},
		{"label with = in the value", []string{"--label", "url=a=b"}, url.Values{"label": {"url=a=b"}}},
		{"deprecated=false", []string{"--deprecated=false"}, url.Values{"deprecated": {"false"}}},
		{"limit 0 is the default", []string{"--limit", "0"}, url.Values{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			pages := onePage(gitlabProduct)
			pages["abc"] = productsPageJSON("", gitlabProduct)
			s := newProductsServer(t, http.StatusOK, pages)
			r := products(t, s, tc.args...)
			if r.err != nil {
				t.Fatalf("products %v: %v", tc.args, r.err)
			}
			reqs := s.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			if !reflect.DeepEqual(reqs[0], tc.want) {
				t.Errorf("query = %v, want %v", reqs[0], tc.want)
			}
		})
	}
}

func TestProductsInvalidFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--label", "nokey"}, {"--label", "=v"}, {"--keyword", ""}, {"--limit", "-1"},
		{"--updated-since", "31/01/2026"}, {"--deprecated=maybe"}, {"extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
			r := products(t, s, args...)
			if !config.IsUsageError(r.err) {
				t.Errorf("products %q = %v, want a usage error", args, r.err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("%d request(s) for invalid flags", n)
			}
		})
	}
}

func threePages() map[string]string {
	return map[string]string{
		"":   productsPageJSON("p2", gitlabProduct),
		"p2": productsPageJSON("p3", bbProduct),
		"p3": productsPageJSON("", sonarProduct),
	}
}

// One page is read without --all: the cursor of the next one is given to the user.
func TestProductsNext(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())

	r := products(t, s, "-o", "json")
	if r.err != nil {
		t.Fatalf("products: %v", r.err)
	}
	got := decodeProducts(t, r.stdout)
	if names := productNames(got); !slices.Equal(names, []string{"gitlab-vcs-plugin"}) || got.Next == nil || *got.Next != "p2" {
		t.Errorf("result = %v next %v, want the first page and next p2", names, got.Next)
	}

	r = products(t, s)
	if r.err != nil || !strings.Contains(r.stderr, "--cursor p2") {
		t.Errorf("stderr = %q (%v), want the cursor of the next page", r.stderr, r.err)
	}
	if strings.Contains(r.stdout, "p2") {
		t.Errorf("stdout = %q: the cursor belongs on stderr", r.stdout)
	}

	if r = products(t, s, "-q"); r.err != nil || r.stderr != "" {
		t.Errorf("-q: stderr = %q (%v), want nothing", r.stderr, r.err)
	}
}

func TestProductsAll(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())
	r := products(t, s, "--all", "--query", "plugin", "-o", "json")
	if r.err != nil {
		t.Fatalf("products --all: %v", r.err)
	}
	got := decodeProducts(t, r.stdout)
	if names := productNames(got); !slices.Equal(names, []string{"gitlab-vcs-plugin", "bb-vcs-plugin", "sonar-plugin"}) {
		t.Errorf("products = %v, want the 3 pages in order", names)
	}
	if got.Next != nil {
		t.Errorf("next = %q after the last page", *got.Next)
	}

	reqs := s.requests()
	var cursors []string
	for _, q := range reqs {
		cursors = append(cursors, q.Get("cursor"))
		if q.Get("q") != "plugin" {
			t.Errorf("query %v lost the criteria", q)
		}
	}
	if !slices.Equal(cursors, []string{"", "p2", "p3"}) {
		t.Errorf("cursors = %q, want the first page, p2 and p3", cursors)
	}
}

func TestProductsAllFromCursor(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())
	r := products(t, s, "--all", "--cursor", "p2", "-o", "json")
	if r.err != nil {
		t.Fatalf("products: %v", r.err)
	}
	if names := productNames(decodeProducts(t, r.stdout)); !slices.Equal(names, []string{"bb-vcs-plugin", "sonar-plugin"}) {
		t.Errorf("products = %v, want from p2 to the end", names)
	}
}

// A server that gives a cursor again would make --all loop forever.
func TestProductsCursorLoop(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, map[string]string{
		"":   productsPageJSON("p2", gitlabProduct),
		"p2": productsPageJSON("p2", bbProduct),
	})
	r := products(t, s, "--all")
	if r.err == nil || !strings.Contains(r.err.Error(), "p2") {
		t.Errorf("products --all on a loop = %v, want an error naming the cursor", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

func TestProductsBadBody(t *testing.T) {
	for name, body := range map[string]string{
		"html": "<html>", "null": "null", "no items": `{"products":[]}`,
		"not objects": "[1, 2]", "item not an object": `{"items":[1]}`, "truncated": `{"items":[`,
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, map[string]string{"": body})
			r := products(t, s, "-o", "json")
			if r.err == nil {
				t.Fatalf("products succeeded on the body %q:\n%s", body, r.stdout)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
		})
	}
}

// A page that fails in the middle of --all prints nothing: not a part of the list.
func TestProductsAllFailsOnAPage(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, map[string]string{"": productsPageJSON("gone", gitlabProduct)})
	r := products(t, s, "--all")
	if r.err == nil || !strings.Contains(r.err.Error(), "400") {
		t.Errorf("products --all = %v, want the 400 of the second page", r.err)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

func TestProductsServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, status, map[string]string{"": `{"error":"nope"}`})
			r := products(t, s, "-o", "json")
			if r.err == nil {
				t.Fatalf("products succeeded on a %d", status)
			}
			if config.IsUsageError(r.err) {
				t.Errorf("%v is a usage error", r.err)
			}
			if msg := r.err.Error(); !strings.Contains(msg, strconv.Itoa(status)) || !strings.Contains(msg, "products") {
				t.Errorf("error %q does not give the status %d", msg, status)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
		})
	}
}

func TestProductsUnreachable(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage())
	s.Close()
	r := products(t, s)
	if r.err == nil || !strings.Contains(r.err.Error(), "connection refused") {
		t.Errorf("products against a closed server = %v, want the cause", r.err)
	}
}

// main prints the error: the command must not log it as well.
func TestProductsErrorPrintedOnce(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusInternalServerError, map[string]string{"": "boom"})
	r := products(t, s)
	if r.err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(r.stderr, "level=ERROR") {
		t.Errorf("the error is logged, and main prints it again:\n%s", r.stderr)
	}
}

func TestProductsVerbose(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
	r := products(t, s, "--query", "git", "-o", "json", "-v")
	if r.err != nil {
		t.Fatal(r.err)
	}
	decodeProducts(t, r.stdout)

	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(r.stderr), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		if rec["level"] == "DEBUG" && rec["msg"] == "request" && rec["url"] == s.URL+"/products?q=git" {
			found = true
			if rec["cmd"] != "products" || rec["method"] != "GET" || rec["status"] != float64(200) {
				t.Errorf("request record = %v, want cmd products, method GET and status 200", rec)
			}
		}
	}
	if !found {
		t.Errorf("no DEBUG 'request' record for /products?q=git with -v:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, token) {
		t.Error("the token is logged")
	}
}

// The flags of a run must not leak into the next one.
func TestProductsRunsIsolated(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
	if r := products(t, s, "--query", "x", "--keyword", "k", "--all"); r.err != nil {
		t.Fatal(r.err)
	}
	if r := products(t, s); r.err != nil {
		t.Fatal(r.err)
	}
	if reqs := s.requests(); len(reqs) != 2 || len(reqs[1]) != 0 {
		t.Errorf("second query = %v, want none", reqs[len(reqs)-1])
	}
}

// The examples of the help must run as written.
func TestProductsExample(t *testing.T) {
	isolate(t)
	pages := threePages()
	pages["<next>"] = productsPageJSON("", sonarProduct)
	s := newProductsServer(t, http.StatusOK, pages)
	for line := range strings.SplitSeq(newProductsCommand().Example, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || fields[0] == "#" {
			continue
		}
		if i := slices.Index(fields, "|"); i >= 0 {
			fields = fields[:i]
		}
		if r := products(t, s, fields[2:]...); r.err != nil {
			t.Errorf("example %q = %v", line, r.err)
		}
	}
}

func TestProductsWithoutRoot(t *testing.T) {
	cmd := newProductsCommand()
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("Execute() = %v, want a 'not loaded' error", err)
	}
}

func TestProductsOutputFails(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
	for _, o := range []string{"text", "json"} {
		root := NewRootCommand()
		root.SetArgs([]string{"products", "--server-url", s.URL, "-o", o})
		root.SetOut(failWriter{})
		root.SetErr(io.Discard)
		if _, err := root.ExecuteC(); !errors.Is(err, errWrite) {
			t.Errorf("-o %s on a failing stdout = %v, want %v", o, err, errWrite)
		}
	}
}
