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
	"github.com/spf13/cobra"
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

type productsRequest struct {
	path  string
	query url.Values
}

type productsServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []productsRequest
}

// newProductsServer answers /products and /products/search with pages[cursor], the cursor ""
// being the first page, and with status when it is not 200. An unknown cursor is a 400.
func newProductsServer(t *testing.T, status int, pages map[string]string) *productsServer {
	t.Helper()
	s := &productsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, productsRequest{r.URL.Path, r.URL.Query()})
		s.mu.Unlock()
		if r.URL.Path != "/products" && r.URL.Path != "/products/search" {
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

func threePages() map[string]string {
	return map[string]string{
		"":   productsPageJSON("p2", gitlabProduct),
		"p2": productsPageJSON("p3", bbProduct),
		"p3": productsPageJSON("", sonarProduct),
	}
}

func (s *productsServer) requests() []productsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func (s *productsServer) paths() []string {
	var paths []string
	for _, r := range s.requests() {
		paths = append(paths, r.path)
	}
	return paths
}

func (s *productsServer) cursors() []string {
	var cursors []string
	for _, r := range s.requests() {
		cursors = append(cursors, r.query.Get("cursor"))
	}
	return cursors
}

// products runs 'products' without retries: a retried 5xx would wait 1s per attempt.
func products(t *testing.T, s *productsServer, args ...string) result {
	t.Helper()
	return run(t, append([]string{"products", "--server-url", s.URL, "--retries", "0"}, args...)...)
}

// search runs 'products search' without retries.
func search(t *testing.T, s *productsServer, args ...string) result {
	t.Helper()
	return run(t, append([]string{"products", "search", "--server-url", s.URL, "--retries", "0"}, args...)...)
}

// both are the two commands, for the tests that hold for each of them.
var both = []struct {
	name string
	path string
	run  func(t *testing.T, s *productsServer, args ...string) result
}{
	{"products", "/products", products},
	{"search", "/products/search", search},
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

// Both commands print the same table, each on its own endpoint.
func TestProductsText(t *testing.T) {
	for _, c := range both {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct, bbProduct))
			r := c.run(t, s)
			if r.err != nil {
				t.Fatalf("%s: %v\nstderr: %s", c.name, r.err, r.stderr)
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
			if paths := s.paths(); !slices.Equal(paths, []string{c.path}) {
				t.Errorf("requests = %q, want one on %s", paths, c.path)
			}
		})
	}
}

// Each product is printed as the server returns it: a field localrectl does not know is kept.
func TestProductsJSON(t *testing.T) {
	for _, c := range both {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct, bbProduct))
			var r result
			if stray := osStdout(t, func() { r = c.run(t, s, "-o", "json") }); stray != "" {
				t.Errorf("%s wrote on os.Stdout, outside of the Printer:\n%s", c.name, stray)
			}
			if r.err != nil {
				t.Fatalf("%s: %v", c.name, r.err)
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
		})
	}
}

func TestProductsEmpty(t *testing.T) {
	for _, c := range both {
		for _, body := range []string{`{"items":[]}`, `[]`} {
			t.Run(c.name+" "+body, func(t *testing.T) {
				isolate(t)
				s := newProductsServer(t, http.StatusOK, map[string]string{"": body})
				r := c.run(t, s, "-o", "json")
				if r.err != nil {
					t.Fatalf("%s: %v", c.name, r.err)
				}
				if got := decodeProducts(t, r.stdout); len(got.Products) != 0 {
					t.Errorf("products = %v, want []", got.Products)
				}
				r = c.run(t, s)
				if r.err != nil || strings.Count(r.stdout, "\n") != 1 {
					t.Errorf("text = %q, %v, want the header alone", r.stdout, r.err)
				}
			})
		}
	}
}

// A server that answers a bare array (no pages) is a single page.
func TestProductsBareArray(t *testing.T) {
	for _, c := range both {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, map[string]string{"": "[" + gitlabProduct + "," + sonarProduct + "]"})
			args := []string{"-o", "json"}
			if c.name == "search" {
				args = append(args, "--all")
			}
			r := c.run(t, s, args...)
			if r.err != nil {
				t.Fatalf("%s: %v", c.name, r.err)
			}
			if names := productNames(decodeProducts(t, r.stdout)); !slices.Equal(names, []string{"gitlab-vcs-plugin", "sonar-plugin"}) {
				t.Errorf("products = %v", names)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

// products lists every product: it follows next up to the last page, without a criterion.
func TestProductsReadsEveryPage(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())
	r := products(t, s, "-o", "json")
	if r.err != nil {
		t.Fatalf("products: %v", r.err)
	}
	got := decodeProducts(t, r.stdout)
	if names := productNames(got); !slices.Equal(names, []string{"gitlab-vcs-plugin", "bb-vcs-plugin", "sonar-plugin"}) {
		t.Errorf("products = %v, want the 3 pages in order", names)
	}
	if got.Next != nil {
		t.Errorf("next = %q, want none: the list is complete", *got.Next)
	}
	if cursors := s.cursors(); !slices.Equal(cursors, []string{"", "p2", "p3"}) {
		t.Errorf("cursors = %q, want the first page, p2 and p3", cursors)
	}
	for _, req := range s.requests() {
		if req.path != "/products" {
			t.Errorf("request on %s, want /products", req.path)
		}
		if req.query.Del("cursor"); len(req.query) != 0 {
			t.Errorf("query = %v, want the cursor alone", req.query)
		}
	}

	if r = products(t, s); r.err != nil || r.stderr != "" {
		t.Errorf("text: stderr = %q (%v), want no cursor hint", r.stderr, r.err)
	}
}

// 'products' takes neither an argument nor a criterion: a typo of search must not list
// everything without a word.
func TestProductsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"extra"}, {"serch"}, {"--query", "x"}, {"--all"}, {"--cursor", "p2"}, {"-k", "vcs"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
			r := products(t, s, args...)
			if !config.IsUsageError(r.err) {
				t.Errorf("products %q = %v, want a usage error", args, r.err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("%d request(s) for a bad use", n)
			}
		})
	}
}

func TestProductsAlias(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
	r := run(t, "products-all", "--server-url", s.URL, "-o", "json")
	if r.err != nil {
		t.Fatalf("products-all: %v", r.err)
	}
	if names := productNames(decodeProducts(t, r.stdout)); !slices.Equal(names, []string{"gitlab-vcs-plugin"}) {
		t.Errorf("products = %v", names)
	}
}

func TestSearchQuery(t *testing.T) {
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
		{"prerelease", []string{"--prerelease"}, url.Values{"prerelease": {"true"}}},
		{"limit 0 is the default", []string{"--limit", "0"}, url.Values{}},
		{"empty strings send nothing", []string{"--query", "", "--owner", "", "--sort", ""}, url.Values{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			pages := onePage(gitlabProduct)
			pages["abc"] = productsPageJSON("", gitlabProduct)
			s := newProductsServer(t, http.StatusOK, pages)
			r := search(t, s, tc.args...)
			if r.err != nil {
				t.Fatalf("search %v: %v", tc.args, r.err)
			}
			reqs := s.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			if reqs[0].path != "/products/search" {
				t.Errorf("path = %s, want /products/search", reqs[0].path)
			}
			if !reflect.DeepEqual(reqs[0].query, tc.want) {
				t.Errorf("query = %v, want %v", reqs[0].query, tc.want)
			}
		})
	}
}

func TestSearchInvalidFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--label", "nokey"}, {"--label", "=v"}, {"--keyword", ""}, {"--limit", "-1"}, {"--limit", "x"},
		{"--updated-since", "31/01/2026"}, {"--updated-since", "2026-13-01"}, {"--deprecated=maybe"},
		{"extra"}, {"--unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
			r := search(t, s, args...)
			if !config.IsUsageError(r.err) {
				t.Errorf("search %q = %v, want a usage error", args, r.err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("%d request(s) for invalid flags", n)
			}
		})
	}
}

// One page is read without --all: the cursor of the next one is given to the user.
func TestSearchNext(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())

	r := search(t, s, "-o", "json")
	if r.err != nil {
		t.Fatalf("search: %v", r.err)
	}
	got := decodeProducts(t, r.stdout)
	if names := productNames(got); !slices.Equal(names, []string{"gitlab-vcs-plugin"}) || got.Next == nil || *got.Next != "p2" {
		t.Errorf("result = %v next %v, want the first page and next p2", names, got.Next)
	}

	r = search(t, s)
	if r.err != nil || !strings.Contains(r.stderr, "--cursor p2") {
		t.Errorf("stderr = %q (%v), want the cursor of the next page", r.stderr, r.err)
	}
	if strings.Contains(r.stdout, "p2") {
		t.Errorf("stdout = %q: the cursor belongs on stderr", r.stdout)
	}

	if r = search(t, s, "-q"); r.err != nil || r.stderr != "" {
		t.Errorf("-q: stderr = %q (%v), want nothing", r.stderr, r.err)
	}

	r = search(t, s, "--cursor", "p2", "-o", "json")
	got = decodeProducts(t, r.stdout)
	if names := productNames(got); r.err != nil || !slices.Equal(names, []string{"bb-vcs-plugin"}) || got.Next == nil || *got.Next != "p3" {
		t.Errorf("--cursor p2 = %v next %v (%v), want the second page and next p3", names, got.Next, r.err)
	}
}

func TestSearchAll(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())
	r := search(t, s, "--all", "--query", "plugin", "-o", "json")
	if r.err != nil {
		t.Fatalf("search --all: %v", r.err)
	}
	got := decodeProducts(t, r.stdout)
	if names := productNames(got); !slices.Equal(names, []string{"gitlab-vcs-plugin", "bb-vcs-plugin", "sonar-plugin"}) {
		t.Errorf("products = %v, want the 3 pages in order", names)
	}
	if got.Next != nil {
		t.Errorf("next = %q after the last page", *got.Next)
	}
	if cursors := s.cursors(); !slices.Equal(cursors, []string{"", "p2", "p3"}) {
		t.Errorf("cursors = %q, want the first page, p2 and p3", cursors)
	}
	for _, req := range s.requests() {
		if req.query.Get("q") != "plugin" {
			t.Errorf("query %v lost the criteria", req.query)
		}
	}
}

func TestSearchAllFromCursor(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())
	r := search(t, s, "--all", "--cursor", "p2", "-o", "json")
	if r.err != nil {
		t.Fatalf("search: %v", r.err)
	}
	if names := productNames(decodeProducts(t, r.stdout)); !slices.Equal(names, []string{"bb-vcs-plugin", "sonar-plugin"}) {
		t.Errorf("products = %v, want from p2 to the end", names)
	}
}

// A server that gives a cursor again would make the reading of every page loop forever.
func TestProductsCursorLoop(t *testing.T) {
	for _, pages := range []map[string]string{
		{"": productsPageJSON("p2", gitlabProduct), "p2": productsPageJSON("p2", bbProduct)},
		{"": productsPageJSON("p2", gitlabProduct), "p2": productsPageJSON("p3", bbProduct), "p3": productsPageJSON("p2", sonarProduct)},
	} {
		for _, c := range both {
			t.Run(c.name, func(t *testing.T) {
				isolate(t)
				s := newProductsServer(t, http.StatusOK, pages)
				var args []string
				if c.name == "search" {
					args = []string{"--all"}
				}
				r := c.run(t, s, args...)
				if r.err == nil || !strings.Contains(r.err.Error(), `"p2" twice`) {
					t.Errorf("%s on a loop = %v, want an error naming the cursor", c.name, r.err)
				}
				if r.stdout != "" {
					t.Errorf("stdout = %q, want nothing", r.stdout)
				}
			})
		}
	}
}

// The cursor given with --cursor counts as seen: a server that sends it back is a loop.
func TestSearchAllCursorGivenBack(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, map[string]string{"p2": productsPageJSON("p2", bbProduct)})
	r := search(t, s, "--all", "--cursor", "p2")
	if r.err == nil || !strings.Contains(r.err.Error(), `"p2" twice`) {
		t.Errorf("search --all --cursor p2 = %v, want a loop error", r.err)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// A page that fails in the middle prints nothing: not a part of the list.
func TestProductsFailsOnAPage(t *testing.T) {
	for _, c := range both {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, map[string]string{"": productsPageJSON("gone", gitlabProduct)})
			var args []string
			if c.name == "search" {
				args = []string{"--all"}
			}
			r := c.run(t, s, args...)
			if r.err == nil || !strings.Contains(r.err.Error(), "400") {
				t.Errorf("%s = %v, want the 400 of the second page", c.name, r.err)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
		})
	}
}

func TestProductsBadBody(t *testing.T) {
	for _, c := range both {
		for name, body := range map[string]string{
			"html": "<html>", "null": "null", "no items": `{"products":[]}`, "empty": "",
			"not objects": "[1, 2]", "item not an object": `{"items":[1]}`, "truncated": `{"items":[`,
		} {
			t.Run(c.name+" "+name, func(t *testing.T) {
				isolate(t)
				s := newProductsServer(t, http.StatusOK, map[string]string{"": body})
				r := c.run(t, s, "-o", "json")
				if r.err == nil {
					t.Fatalf("%s succeeded on the body %q:\n%s", c.name, body, r.stdout)
				}
				if r.stdout != "" {
					t.Errorf("stdout = %q, want nothing", r.stdout)
				}
			})
		}
	}
}

func TestProductsServerErrors(t *testing.T) {
	for _, c := range both {
		for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
			t.Run(c.name+" "+http.StatusText(status), func(t *testing.T) {
				isolate(t)
				s := newProductsServer(t, status, map[string]string{"": `{"error":"nope"}`})
				r := c.run(t, s, "-o", "json")
				if r.err == nil {
					t.Fatalf("%s succeeded on a %d", c.name, status)
				}
				if config.IsUsageError(r.err) {
					t.Errorf("%v is a usage error", r.err)
				}
				msg := r.err.Error()
				if !strings.Contains(msg, strconv.Itoa(status)) || !strings.Contains(msg, s.URL+c.path) {
					t.Errorf("error %q does not give the status %d and the url", msg, status)
				}
				if r.stdout != "" {
					t.Errorf("stdout = %q, want nothing", r.stdout)
				}
			})
		}
	}
}

// Each command names what it was doing.
func TestProductsErrorAction(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		run        func(t *testing.T, s *productsServer, args ...string) result
	}{
		{"products", "fetch the products", products},
		{"search", "search the products", search},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusInternalServerError, onePage())
			if r := tc.run(t, s); r.err == nil || !strings.HasPrefix(r.err.Error(), tc.want) {
				t.Errorf("%s = %v, want an error starting with %q", tc.name, r.err, tc.want)
			}
		})
	}
}

func TestProductsUnreachable(t *testing.T) {
	for _, c := range both {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage())
			s.Close()
			r := c.run(t, s)
			if r.err == nil || !strings.Contains(r.err.Error(), "connection refused") {
				t.Errorf("%s against a closed server = %v, want the cause", c.name, r.err)
			}
		})
	}
}

// main prints the error: the command must not log it as well.
func TestProductsErrorPrintedOnce(t *testing.T) {
	for _, c := range both {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusInternalServerError, map[string]string{"": "boom"})
			r := c.run(t, s)
			if r.err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(r.stderr, "level=ERROR") {
				t.Errorf("the error is logged, and main prints it again:\n%s", r.stderr)
			}
		})
	}
}

func TestProductsVerbose(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, url string
		run            func(t *testing.T, s *productsServer, args ...string) result
		args           []string
	}{
		{"products", "products", "/products", products, nil},
		{"search", "products search", "/products/search?q=git", search, []string{"--query", "git"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
			t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
			r := tc.run(t, s, append(tc.args, "-o", "json", "-v")...)
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
				if rec["level"] == "DEBUG" && rec["msg"] == "request" && rec["url"] == s.URL+tc.url {
					found = true
					if rec["cmd"] != tc.cmd || rec["method"] != "GET" || rec["status"] != float64(200) {
						t.Errorf("request record = %v, want cmd %q, method GET and status 200", rec, tc.cmd)
					}
				}
			}
			if !found {
				t.Errorf("no DEBUG 'request' record for %s with -v:\n%s", tc.url, r.stderr)
			}
			if strings.Contains(r.stderr, token) {
				t.Error("the token is logged")
			}
			for _, req := range s.requests() {
				if req.query.Get("token") != "" {
					t.Errorf("the token is in the query %v", req.query)
				}
			}
		})
	}
}

// The response body of a failed request is logged with -v, to tell why the server refused.
func TestProductsVerboseErrorBody(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusBadRequest, map[string]string{"": `{"error":"bad sort"}`})
	r := search(t, s, "--sort", "nope", "-v")
	if r.err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(r.stderr, "bad sort") {
		t.Errorf("stderr = %q, want the body of the response", r.stderr)
	}
}

// The flags of a run must not leak into the next one.
func TestSearchRunsIsolated(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
	if r := search(t, s, "--query", "x", "--keyword", "k", "--deprecated", "--all"); r.err != nil {
		t.Fatal(r.err)
	}
	if r := search(t, s); r.err != nil {
		t.Fatal(r.err)
	}
	if reqs := s.requests(); len(reqs) != 2 || len(reqs[1].query) != 0 {
		t.Errorf("second query = %v, want none", reqs[len(reqs)-1].query)
	}
}

// The examples of the help must run as written.
func TestProductsExamples(t *testing.T) {
	for _, cmd := range []*cobra.Command{newProductsCommand(), newSearchCommand()} {
		t.Run(cmd.Name(), func(t *testing.T) {
			isolate(t)
			pages := threePages()
			pages["<next>"] = productsPageJSON("", sonarProduct)
			s := newProductsServer(t, http.StatusOK, pages)
			for line := range strings.SplitSeq(cmd.Example, "\n") {
				fields := strings.Fields(strings.TrimSpace(line))
				if len(fields) == 0 || fields[0] == "#" {
					continue
				}
				if i := slices.Index(fields, "|"); i >= 0 {
					fields = fields[:i]
				}
				if fields[0] != "localrectl" {
					t.Fatalf("example %q does not start with localrectl", line)
				}
				args := append(fields[1:], "--server-url", s.URL, "--retries", "0")
				if r := run(t, args...); r.err != nil {
					t.Errorf("example %q = %v", line, r.err)
				}
			}
		})
	}
}

// search is a subcommand of products, and the help of each one points to the other.
func TestProductsHelp(t *testing.T) {
	isolate(t)
	r := run(t, "products", "--help")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(r.stdout, "products search") || !strings.Contains(r.stdout, "Available Commands") {
		t.Errorf("products --help does not list search:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "-Q, --query") {
		t.Errorf("products --help shows the flags of search:\n%s", r.stdout)
	}

	r = run(t, "products", "search", "--help")
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, flag := range []string{"--query", "--keyword", "--label", "--cursor", "--all", "updatedSince"} {
		if !strings.Contains(r.stdout, flag) {
			t.Errorf("products search --help does not hold %s", flag)
		}
	}
}

func TestProductsWithoutRoot(t *testing.T) {
	for _, cmd := range []*cobra.Command{newProductsCommand(), newSearchCommand()} {
		t.Run(cmd.Name(), func(t *testing.T) {
			cmd.SetArgs(nil)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not loaded") {
				t.Errorf("Execute() = %v, want a 'not loaded' error", err)
			}
		})
	}
}

func TestProductsOutputFails(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, onePage(gitlabProduct))
	for _, args := range [][]string{{"products"}, {"products", "search"}} {
		for _, o := range []string{"text", "json"} {
			root := NewRootCommand()
			root.SetArgs(append(args, "--server-url", s.URL, "-o", o))
			root.SetOut(failWriter{})
			root.SetErr(io.Discard)
			if _, err := root.ExecuteC(); !errors.Is(err, errWrite) {
				t.Errorf("%v -o %s on a failing stdout = %v, want %v", args, o, err, errWrite)
			}
		}
	}
}

// The cursor hint comes only after the table: a failed output gives no hint.
func TestSearchNoHintOnFailedOutput(t *testing.T) {
	isolate(t)
	s := newProductsServer(t, http.StatusOK, threePages())
	var stderr strings.Builder
	root := NewRootCommand()
	root.SetArgs([]string{"products", "search", "--server-url", s.URL})
	root.SetOut(failWriter{})
	root.SetErr(&stderr)
	if _, err := root.ExecuteC(); !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want %v", err, errWrite)
	}
	if strings.Contains(stderr.String(), "--cursor") {
		t.Errorf("stderr = %q, want no hint after a failed output", stderr.String())
	}
}
