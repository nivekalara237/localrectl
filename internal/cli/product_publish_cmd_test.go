package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fullServer is a fake server for the whole publish command: the product schema, the GET of
// the release (release is its status), the PUT of the files and the POST publish.
type fullServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newFullServer(t *testing.T, release int) *fullServer {
	t.Helper()
	s := &fullServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		switch {
		case r.URL.Path == productSchemaPath:
			_, _ = io.WriteString(w, productSchema)
		case r.Method == http.MethodGet:
			w.WriteHeader(release)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			_, _ = io.WriteString(w, `{"product":"billing","version":"1.2.0","files":["a.txt"]}`)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// sent returns the PUT and POST requests.
func (s *fullServer) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.requests), func(r string) bool { return strings.HasPrefix(r, "GET ") })
}

// publishDir returns a directory with a.txt and bb.txt, and a valid product card beside it.
func publishDir(t *testing.T) (root, productCard string) {
	t.Helper()
	dir := t.TempDir()
	root = filepath.Join(dir, "out")
	write(t, filepath.Join(root, "a.txt"), "a")
	write(t, filepath.Join(root, "bb.txt"), "bb")
	return root, write(t, filepath.Join(dir, "registry.yaml"), "name: billing\n")
}

// publish runs localrectl publish against url, with the token in the environment and no
// retries (a retried 5xx would wait 1s per attempt).
func publish(t *testing.T, url string, args ...string) result {
	t.Helper()
	t.Setenv("LOCAL_REGISTRY_UPLOAD_TOKEN", token)
	return run(t, append([]string{"publish", "billing", "--server-url", url, "--retries", "0"}, args...)...)
}

func TestPublishCommand(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)

	r := publish(t, srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0", "--latest", "--no-color")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	if r.stdout != "billing 1.2.0: PUBLISHED, 3 files (17 B)\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	want := []string{
		"PUT /billing/artifacts/1.2.0/files/a.txt", "PUT /billing/artifacts/1.2.0/files/bb.txt",
		"PUT /billing/artifacts/1.2.0/files/registry.yaml", "POST /billing/artifacts/1.2.0/publish",
	}
	if !slices.Equal(srv.sent(), want) {
		t.Errorf("sent %v", srv.sent())
	}
	if !strings.Contains(r.stderr, "[3/3] registry.yaml  14 B  ✓") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestPublishCommandJSON(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)

	r := publish(t, srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0", "-o", "json")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	var got summary
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("%v: %q", err, r.stdout)
	}
	if want := (summary{Total: 3, TotalSize: 17, Status: "PUBLISHED", Version: "1.2.0", Product: "billing"}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

func TestPublishCommandCurrentDir(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)
	t.Chdir(root) // no --root: the current directory

	r := publish(t, srv.URL, "--product-file", productCard, "--version", "1.2.0", "--dry-run")
	if r.err != nil || r.stdout != "billing 1.2.0: DRY-RUN, 3 files (17 B)\n" {
		t.Errorf("stdout = %q, err = %v", r.stdout, r.err)
	}
	if len(srv.sent()) != 0 {
		t.Errorf("dry run sent %v", srv.sent())
	}
}

func TestPublishCommandInvalidCard(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, _ := publishDir(t)
	productCard := write(t, filepath.Join(t.TempDir(), "card.yaml"), "name: 1\n")

	r := publish(t, srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0")
	if r.err == nil || !strings.Contains(r.err.Error(), "the product card "+productCard+" is not valid:\n  name: ") {
		t.Fatalf("err = %v", r.err)
	}
	if len(srv.sent()) != 0 {
		t.Errorf("sent %v with a card that is not valid", srv.sent())
	}

	// --no-validate sends it as it is.
	r = publish(t, srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0", "--no-validate")
	if r.err != nil {
		t.Errorf("--no-validate: %v", r.err)
	}
}

func TestPublishCommandUnreachableSchema(t *testing.T) {
	isolate(t)
	root, productCard := publishDir(t)
	r := publish(t, "http://127.0.0.1:1", "--root", root, "--product-file", productCard, "--version", "1.2.0")
	if r.err == nil || !strings.Contains(r.err.Error(), "product schema") {
		t.Errorf("err = %v", r.err)
	}
}

func TestPublishCommandFlags(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)
	dir := t.TempDir()
	notDir := write(t, filepath.Join(dir, "file"), "x")
	emptyDir := filepath.Join(dir, "empty")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	onlyArtifacts := filepath.Join(dir, "only-artifacts")
	write(t, filepath.Join(onlyArtifacts, "artifacts.json"), "[]")

	base := []string{"--root", root, "--product-file", productCard, "--version", "1.2.0"}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"if-exists", []string{"--if-exists", "always"}, `--if-exists must be fail or skip, not "always"`},
		{"parallel", []string{"--parallel", "0"}, "--parallel must be 1 or more, not 0"},
		{"changelog", []string{"--changelog-file", filepath.Join(dir, "none.md")}, "the changelog file " + filepath.Join(dir, "none.md") + " does not exist"},
		{"product card", []string{"--product-file", filepath.Join(dir, "none.yaml")}, "the product card file " + filepath.Join(dir, "none.yaml") + " does not exist"},
		{"empty product card", []string{"--product-file", ""}, "the product card is required: pass --product-file"},
		{"root missing", []string{"--root", filepath.Join(dir, "none")}, "the directory " + filepath.Join(dir, "none") + " does not exist"},
		{"root not a directory", []string{"--root", notDir}, "--root " + notDir + " is not a directory"},
		{"compress type", []string{"--compress-type", "gz"}, `--compress-type must be zip or tar.gz, not "gz"`},
		{"semver", []string{"--version", "v1.2"}, "--version v1.2 is not X.Y.Z or X.Y.Z-pre"},
		{"artifacts file", []string{"--goreleaser-artifacts-file", filepath.Join(dir, "a.json")}, "the goreleaser artifacts file " + filepath.Join(dir, "a.json") + " does not exist"},
		{"metadata file", []string{"--goreleaser-metadata-file", filepath.Join(dir, "m.json")}, "the goreleaser metadata file " + filepath.Join(dir, "m.json") + " does not exist"},
		{"goreleaser dir missing", []string{"--goreleaser-dir", filepath.Join(dir, "none")}, "--goreleaser-dir: the directory"},
		{"goreleaser dir not a directory", []string{"--goreleaser-dir", notDir}, "--goreleaser-dir " + notDir + " is not a directory"},
		{"no artifacts.json", []string{"--goreleaser-dir", emptyDir}, "--goreleaser-dir: no artifacts.json in " + emptyDir},
		{"no metadata.json", []string{"--goreleaser-dir", onlyArtifacts}, "--goreleaser-dir: no metadata.json in " + onlyArtifacts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := publish(t, srv.URL, append(slices.Clone(base), tc.args...)...)
			if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
				t.Errorf("err = %v\nwant  %s", r.err, tc.want)
			}
		})
	}
	if len(srv.sent()) != 0 {
		t.Errorf("sent %v with bad flags", srv.sent())
	}
}

func TestPublishCommandRequiresProductFile(t *testing.T) {
	isolate(t)
	r := run(t, "publish", "billing")
	if r.err == nil || !strings.Contains(r.err.Error(), `"product-file" not set`) {
		t.Errorf("err = %v", r.err)
	}
}

func TestPublishCommandSubdirectories(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)
	write(t, filepath.Join(root, "linux", "app"), "app")
	base := []string{"--root", root, "--product-file", productCard, "--version", "1.2.0", "--dry-run"}

	r := publish(t, srv.URL, base...)
	if r.err == nil || !strings.Contains(r.err.Error(), "linux is a directory: archive it first (tar czf linux.tar.gz -C "+root+" linux)") {
		t.Errorf("err = %v", r.err)
	}

	r = publish(t, srv.URL, append(slices.Clone(base), "--skip-subdirs")...)
	if r.err != nil || !strings.Contains(r.stdout, "3 files") {
		t.Errorf("--skip-subdirs: stdout = %q, err = %v", r.stdout, r.err)
	}

	r = publish(t, srv.URL, append(slices.Clone(base), "--compress-subdirs")...)
	if r.err != nil || !strings.Contains(r.stdout, "4 files") || !strings.Contains(r.stderr, "linux.tar.gz") {
		t.Errorf("--compress-subdirs: stdout = %q, stderr = %q, err = %v", r.stdout, r.stderr, r.err)
	}
}

func TestPublishCommandReservedName(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)
	write(t, filepath.Join(root, "release.json"), "{}")

	r := publish(t, srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0", "--dry-run")
	if r.err == nil || !strings.Contains(r.err.Error(), "release.json is a reserved name") {
		t.Errorf("err = %v", r.err)
	}
}

// goreleaserDist writes a goreleaser build in dist under a new directory, and returns the
// directory: artifacts.json lists dist/app.tar.gz.
func goreleaserDist(t *testing.T, metadata string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "dist", "app.tar.gz"), "app")
	write(t, filepath.Join(dir, "dist", "artifacts.json"), `[{"name":"app.tar.gz","path":"dist/app.tar.gz","type":"Archive"}]`)
	write(t, filepath.Join(dir, "dist", "metadata.json"), metadata)
	return dir
}

func TestPublishCommandGoreleaserDir(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	_, productCard := publishDir(t)
	dir := goreleaserDist(t, `{"tag":"v1.2.0","version":"1.2.0"}`)

	// No --version: the tag of metadata.json. The file flags are ignored with --goreleaser-dir.
	r := publish(t, srv.URL, "--goreleaser-dir", filepath.Join(dir, "dist"), "--product-file", productCard,
		"--goreleaser-artifacts-file", "x.json", "--goreleaser-metadata-file", "y.json")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	want := []string{
		"PUT /billing/artifacts/v1.2.0/files/app.tar.gz", "PUT /billing/artifacts/v1.2.0/files/artifacts.json",
		"PUT /billing/artifacts/v1.2.0/files/metadata.json", "PUT /billing/artifacts/v1.2.0/files/registry.yaml",
		"POST /billing/artifacts/v1.2.0/publish",
	}
	if !slices.Equal(srv.sent(), want) {
		t.Errorf("sent %v", srv.sent())
	}
	for _, flag := range []string{"--goreleaser-artifacts-file is ignored", "--goreleaser-metadata-file is ignored"} {
		if !strings.Contains(r.stderr, flag) {
			t.Errorf("no warning %q: %s", flag, r.stderr)
		}
	}
}

func TestPublishCommandGoreleaserInRoot(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	_, productCard := publishDir(t)
	dir := goreleaserDist(t, `{"version":"1.2.0"}`)

	// The root holds an artifacts.json: a goreleaser build, found alone.
	r := publish(t, srv.URL, "--root", filepath.Join(dir, "dist"), "--product-file", productCard, "--dry-run", "-v")
	if r.err != nil || r.stdout != "billing 1.2.0: DRY-RUN, 4 files (101 B)\n" {
		t.Errorf("stdout = %q, err = %v\nstderr: %s", r.stdout, r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "artifacts.json found in the root directory") {
		t.Errorf("stderr = %s", r.stderr)
	}
}

func TestPublishCommandGoreleaserFiles(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	_, productCard := publishDir(t)
	dir := goreleaserDist(t, `{"tag":"v1.2.0"}`)

	r := publish(t, srv.URL, "--root", dir, "--product-file", productCard, "--dry-run",
		"--goreleaser-artifacts-file", filepath.Join(dir, "dist", "artifacts.json"),
		"--goreleaser-metadata-file", filepath.Join(dir, "dist", "metadata.json"))
	if r.err != nil || !strings.HasPrefix(r.stdout, "billing v1.2.0: DRY-RUN, 4 files") {
		t.Errorf("stdout = %q, err = %v\nstderr: %s", r.stdout, r.err, r.stderr)
	}

	bad := write(t, filepath.Join(t.TempDir(), "artifacts.json"), `"a string"`)
	r = publish(t, srv.URL, "--root", dir, "--product-file", productCard, "--goreleaser-artifacts-file", bad)
	if r.err == nil || !strings.Contains(r.err.Error(), bad+" is not valid:") {
		t.Errorf("err = %v", r.err)
	}
}

func TestPublishCommandAlreadyPublished(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusOK)
	root, productCard := publishDir(t)
	base := []string{"--root", root, "--product-file", productCard, "--version", "1.2.0"}

	r := publish(t, srv.URL, base...)
	if r.err == nil || !strings.Contains(r.err.Error(), "already published") {
		t.Errorf("err = %v", r.err)
	}
	r = publish(t, srv.URL, append(slices.Clone(base), "--if-exists", "skip")...)
	if r.err != nil || r.stdout != "billing 1.2.0: SKIPPED, 0 files (0 B)\n" {
		t.Errorf("stdout = %q, err = %v", r.stdout, r.err)
	}
}

func TestPublishCommandChangelog(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)
	changelog := write(t, filepath.Join(t.TempDir(), "NOTES.md"), "## 1.2.0\n")

	r := publish(t, srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0", "--changelog-file", changelog)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !slices.Contains(srv.sent(), "PUT /billing/artifacts/1.2.0/files/CHANGELOG.md") {
		t.Errorf("sent %v", srv.sent())
	}
}

func TestPublishCommandWithoutToken(t *testing.T) {
	isolate(t)
	srv := newFullServer(t, http.StatusNotFound)
	root, productCard := publishDir(t)

	r := run(t, "publish", "billing", "--server-url", srv.URL, "--root", root, "--product-file", productCard, "--version", "1.2.0")
	if r.err == nil || !strings.Contains(r.err.Error(), "no upload token") {
		t.Errorf("err = %v", r.err)
	}
}

func TestPublishCommandHelp(t *testing.T) {
	isolate(t)
	r := mustRun(t, "publish", "--help")
	for _, want := range []string{"localrectl publish <product-name> [flags]", "--if-exists", "registry.yaml", "Examples:"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help has no %q", want)
		}
	}
}
