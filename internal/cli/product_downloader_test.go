package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/util"
)

// downloadServer is a fake server for the download command: the release.json of billing 1.2.0,
// also served as latest, and its files. served replaces the content of a file on download,
// failures gives a file an error status, and cuts closes the connection of a file: before the
// response when its value is 0, else after that many bytes of the body. rawRelease replaces the
// release.json.
type downloadServer struct {
	*httptest.Server
	mu         sync.Mutex
	artifacts  []map[string]any
	contents   map[string]string
	served     map[string]string
	checksums  map[string]any
	failures   map[string]int
	cuts       map[string]int
	rawRelease string
	requests   []string
}

func newDownloadServer(t *testing.T) *downloadServer {
	t.Helper()
	s := &downloadServer{contents: map[string]string{}, served: map[string]string{}, checksums: map[string]any{"name": "checksums.txt"},
		failures: map[string]int{}, cuts: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r.URL.Path)
		switch r.URL.Path {
		case "/billing/releases/1.2.0", "/billing/releases/latest":
			if s.rawRelease != "" {
				_, _ = io.WriteString(w, s.rawRelease)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"product": "billing", "version": "1.2.0", "artifacts": s.artifacts,
				"checksums": s.checksums,
			})
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/billing/artifacts/1.2.0/files/")
		content, found := s.contents[name]
		if !ok || !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
		if replaced, tampered := s.served[name]; tampered {
			content = replaced
		}
		if status, failed := s.failures[name]; failed {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"disk full"}`)
			return
		}
		if sent, cut := s.cuts[name]; cut {
			if sent == 0 {
				connection, _, _ := w.(http.Hijacker).Hijack()
				_ = connection.Close()
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = io.WriteString(w, content[:sent])
			return
		}
		_, _ = io.WriteString(w, content)
	}))
	t.Cleanup(s.Close)
	return s
}

// add lists a file in the release.json; an empty os or arch is null.
func (s *downloadServer) add(name, kind, osName, arch, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact := map[string]any{"name": name, "kind": kind, "size": len(content), "sha256": util.Sha256String(content)}
	if osName != "" {
		artifact["os"] = osName
	}
	if arch != "" {
		artifact["arch"] = arch
	}
	s.artifacts = append(s.artifacts, artifact)
	s.contents[name] = content
}

func (s *downloadServer) fileRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.requests), func(path string) bool { return strings.Contains(path, "/releases/") })
}

// newBillingRelease serves the archives of this machine (tar.gz and zip), the archive of another
// platform, a linux arm64 binary, a file with no platform and the checksums file.
func newBillingRelease(t *testing.T) *downloadServer {
	t.Helper()
	s := newDownloadServer(t)
	platform := runtime.GOOS + "_" + runtime.GOARCH
	s.add("billing_"+platform+".tar.gz", "archive", runtime.GOOS, runtime.GOARCH, "targz")
	s.add("billing_"+platform+".zip", "archive", runtime.GOOS, runtime.GOARCH, "zip")
	s.add("billing_plan9_mips.tar.gz", "archive", "plan9", "mips", "other")
	s.add("billing_linux_arm64", "binary", "linux", "arm64", "binary")
	s.add("README.md", "file", "", "", "readme")
	s.contents["checksums.txt"] = "sums"
	return s
}

func download(t *testing.T, url string, args ...string) result {
	t.Helper()
	return run(t, append([]string{"download", "billing", "--server-url", url, "--retries", "0", "--no-color"}, args...)...)
}

func noPartFiles(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".part-") {
			t.Errorf("temporary file left: %s", entry.Name())
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestDownloadDefaultArchive(t *testing.T) {
	dir := isolate(t)
	srv := newBillingRelease(t)

	r := download(t, srv.URL)
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	name, content := "billing_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz", "targz"
	if runtime.GOOS == "windows" {
		name, content = strings.TrimSuffix(name, ".tar.gz")+".zip", "zip"
	}
	if got := readFile(t, filepath.Join(dir, name)); got != content {
		t.Errorf("%s = %q", name, got)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	want := fmt.Sprintf("billing 1.2.0: 1 files (%d B) in %s\n", len(content), realDir)
	if got := strings.Replace(r.stdout, dir, realDir, 1); got != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	if !strings.Contains(r.stderr, "[1/1] "+name) {
		t.Errorf("stderr = %q", r.stderr)
	}
	noPartFiles(t, dir)
}

func TestDownloadAll(t *testing.T) {
	isolate(t)
	srv := newBillingRelease(t)
	dest := filepath.Join(t.TempDir(), "a", "b")

	r := download(t, srv.URL, "1.2.0", "--all", "-d", dest, "-o", "json")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	var summary downloadSummary
	if err := json.Unmarshal([]byte(r.stdout), &summary); err != nil {
		t.Fatalf("%v: %q", err, r.stdout)
	}
	if len(summary.Files) != 6 || summary.Dir != dest || summary.Version != "1.2.0" || summary.DryRun {
		t.Fatalf("summary = %+v", summary)
	}
	checksums := summary.Files[5]
	if checksums.Name != "checksums.txt" || checksums.Kind != kindChecksums || checksums.Verified || checksums.Status != statusDownloaded {
		t.Errorf("checksums file = %+v", checksums)
	}
	if !summary.Files[0].Verified || summary.Files[0].Sha256 != util.Sha256String("targz") {
		t.Errorf("first file = %+v", summary.Files[0])
	}
	if got := readFile(t, filepath.Join(dest, "checksums.txt")); got != "sums" {
		t.Errorf("checksums.txt = %q", got)
	}
	if info, err := os.Stat(filepath.Join(dest, "README.md")); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("README.md: %v %v", info, err)
	}
	noPartFiles(t, dest)
}

func TestDownloadFiles(t *testing.T) {
	dir := isolate(t)
	srv := newBillingRelease(t)

	mustRun(t, "download", "billing", "--server-url", srv.URL, "--file", "README.md", "--file", "checksums.txt")
	if got := srv.fileRequests(); !slices.Equal(got, []string{"/billing/artifacts/1.2.0/files/README.md", "/billing/artifacts/1.2.0/files/checksums.txt"}) {
		t.Errorf("requests = %v", got)
	}
	if got := readFile(t, filepath.Join(dir, "README.md")); got != "readme" {
		t.Errorf("README.md = %q", got)
	}

	r := download(t, srv.URL, "--file", "missing.zip")
	if r.err == nil || !strings.Contains(r.err.Error(), "has no file missing.zip; its files: billing_") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadFilters(t *testing.T) {
	dir := isolate(t)
	srv := newBillingRelease(t)

	r := download(t, srv.URL, "--os", "linux", "--arch", "arm64", "--kind", "binary")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	info, err := os.Stat(filepath.Join(dir, "billing_linux_arm64"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("binary mode = %v", info.Mode().Perm())
	}

	r = download(t, srv.URL, "--kind", "file", "--os", "macos")
	if r.err != nil || readFile(t, filepath.Join(dir, "README.md")) != "readme" {
		t.Errorf("a file with no platform: %v", r.err)
	}

	r = download(t, srv.URL, "--os", "windows", "--arch", "arm64")
	if r.err == nil || !strings.Contains(r.err.Error(), "has no archive for windows/arm64") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadAmbiguousArchive(t *testing.T) {
	isolate(t)
	srv := newDownloadServer(t)
	srv.add("billing-a.tar.gz", "archive", runtime.GOOS, runtime.GOARCH, "a")
	srv.add("billing-b.tar.gz", "archive", runtime.GOOS, runtime.GOARCH, "b")
	srv.add("billing-a.zip", "archive", runtime.GOOS, runtime.GOARCH, "c")
	srv.add("billing-b.zip", "archive", runtime.GOOS, runtime.GOARCH, "d")

	r := download(t, srv.URL)
	if r.err == nil || !strings.Contains(r.err.Error(), "several archives") {
		t.Errorf("err = %v", r.err)
	}
	if len(srv.fileRequests()) != 0 {
		t.Errorf("downloaded %v", srv.fileRequests())
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	dir := isolate(t)
	srv := newBillingRelease(t)
	srv.served["README.md"] = "README"

	r := download(t, srv.URL, "--file", "README.md")
	if r.err == nil || !strings.Contains(r.err.Error(), "README.md: sha256 mismatch") {
		t.Fatalf("err = %v", r.err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); !os.IsNotExist(err) {
		t.Errorf("a corrupted file reached the destination: %v", err)
	}
	if !strings.Contains(r.stderr, "✗") {
		t.Errorf("stderr = %q", r.stderr)
	}
	noPartFiles(t, dir)

	srv.served["README.md"] = "readme+"
	r = download(t, srv.URL, "--file", "README.md")
	if r.err == nil || !strings.Contains(r.err.Error(), "7 bytes received, the release gives 6") {
		t.Errorf("err = %v", r.err)
	}

	r = download(t, srv.URL, "--file", "README.md", "--no-verify")
	if r.err != nil || readFile(t, filepath.Join(dir, "README.md")) != "readme+" {
		t.Errorf("--no-verify: %v", r.err)
	}
	if !strings.Contains(r.stderr, "--no-verify") {
		t.Errorf("no warning: %q", r.stderr)
	}
}

func TestDownloadExistingFile(t *testing.T) {
	dir := isolate(t)
	srv := newBillingRelease(t)

	write(t, filepath.Join(dir, "README.md"), "readme")
	r := download(t, srv.URL, "--file", "README.md")
	if r.err != nil || !strings.Contains(r.stderr, "README.md  6 B  already up to date") {
		t.Errorf("err = %v, stderr = %q", r.err, r.stderr)
	}
	if len(srv.fileRequests()) != 0 {
		t.Errorf("downloaded again: %v", srv.fileRequests())
	}

	write(t, filepath.Join(dir, "README.md"), "old")
	r = download(t, srv.URL, "--file", "README.md")
	if r.err == nil || !strings.Contains(r.err.Error(), "pass --force to replace it") {
		t.Errorf("err = %v", r.err)
	}
	if readFile(t, filepath.Join(dir, "README.md")) != "old" {
		t.Error("the file was replaced without --force")
	}

	r = download(t, srv.URL, "--file", "README.md", "--force")
	if r.err != nil || readFile(t, filepath.Join(dir, "README.md")) != "readme" {
		t.Errorf("--force: %v", r.err)
	}

	if err := os.Mkdir(filepath.Join(dir, "checksums.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	r = download(t, srv.URL, "--file", "checksums.txt", "--force")
	if r.err == nil || !strings.Contains(r.err.Error(), "is not a regular file") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadDryRun(t *testing.T) {
	isolate(t)
	srv := newBillingRelease(t)
	dest := filepath.Join(t.TempDir(), "missing")

	r := download(t, srv.URL, "--all", "--dry-run", "-d", dest)
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a dry run created %s", dest)
	}
	if len(srv.fileRequests()) != 0 {
		t.Errorf("downloaded %v", srv.fileRequests())
	}
	if !strings.Contains(r.stderr, "checksums.txt  0 B  sha256 unknown (not downloaded: dry run)") ||
		!strings.HasSuffix(r.stdout, "(dry run)\n") {
		t.Errorf("stdout = %q\nstderr = %q", r.stdout, r.stderr)
	}
}

func TestDownloadReleaseErrors(t *testing.T) {
	dir := isolate(t)
	srv := newDownloadServer(t)
	srv.add("../evil", "file", "", "", "evil")
	srv.add("nosum.txt", "file", "", "", "x")
	srv.add("gone.txt", "file", "", "", "gone")
	srv.artifacts[1]["sha256"] = ""
	delete(srv.contents, "gone.txt")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--file", "../evil"}, `invalid file name "../evil"`},
		{[]string{"--file", "nosum.txt"}, "no sha256 for nosum.txt: pass --no-verify"},
		{[]string{"--file", "gone.txt"}, "download gone.txt: not found on the server"},
		{[]string{"1.0.0"}, "billing or 1.0.0 not found"},
	} {
		r := download(t, srv.URL, tc.args...)
		if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, r.err, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); !os.IsNotExist(err) {
		t.Error("a file was written outside the destination")
	}

	write(t, filepath.Join(dir, "file"), "")
	r := download(t, srv.URL, "--file", "nosum.txt", "-d", filepath.Join(dir, "file"))
	if r.err == nil || !strings.Contains(r.err.Error(), "is not a directory") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadUsageErrors(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"download"},
		{"download", "billing", "1.2.0", "extra"},
		{"download", "../billing"},
		{"download", "billing", ".."},
		{"download", "billing", "--os", "beos"},
		{"download", "billing", "--kind", "image"},
		{"download", "billing", "--all", "--file", "a"},
		{"download", "billing", "--all", "--os", "linux"},
		{"download", "billing", "--file", "a", "--kind", "file"},
		{"download", "billing", "--verify-signature"},
	} {
		r := run(t, append(args, "--server-url", "http://127.0.0.1:1")...)
		if !config.IsUsageError(r.err) {
			t.Errorf("%v: err = %v, want a usage error", args, r.err)
		}
	}
}

func TestSafeFileName(t *testing.T) {
	for _, name := range []string{"", ".", "..", ".hidden", "../x", "a/b", `a\b`, "/etc/passwd"} {
		if safeFileName(name) == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if err := safeFileName("billing_1.2.0_linux_amd64.tar.gz"); err != nil {
		t.Error(err)
	}
}

func TestArchiveFormat(t *testing.T) {
	zip := "zip"
	for _, tc := range []struct {
		artifact releaseArtifact
		want     string
	}{
		{releaseArtifact{Name: "a.tar.gz"}, "tar.gz"},
		{releaseArtifact{Name: "a.tgz"}, "tar.gz"},
		{releaseArtifact{Name: "a.zip"}, "zip"},
		{releaseArtifact{Name: "a.tar.gz", Format: &zip}, "zip"},
		{releaseArtifact{Name: "a.deb"}, ""},
	} {
		if got := tc.artifact.archiveFormat(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.artifact, got, tc.want)
		}
	}
}

// tarGz returns a gzip-compressed tar of files, by name; a name ending with / is a directory.
func tarGz(t *testing.T, files map[string]string) string {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(files[name])), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			header.Typeflag = tar.TypeDir
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}

// zipOf returns a zip of files, by name.
func zipOf(t *testing.T, files map[string]string) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(content, files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}

func noExtractDirs(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".extract-") {
			t.Errorf("temporary directory left: %s", entry.Name())
		}
	}
}

func newArchivesRelease(t *testing.T) *downloadServer {
	t.Helper()
	srv := newDownloadServer(t)
	srv.add("billing.tar.gz", "archive", "", "", tarGz(t, map[string]string{"billing/": "", "billing/bin/billing": "binary"}))
	srv.add("billing.zip", "archive", "", "", zipOf(t, map[string]string{"docs/a.txt": "a"}))
	srv.add("README.md", "file", "", "", "readme")
	return srv
}

func TestDownloadExtract(t *testing.T) {
	isolate(t)
	srv := newArchivesRelease(t)
	dest := filepath.Join(t.TempDir(), "opt")
	args := []string{"--file", "billing.tar.gz", "--file", "billing.zip", "--file", "README.md", "--extract", "-d", dest}

	r := download(t, srv.URL, append(args, "-o", "json")...)
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	var summary downloadSummary
	if err := json.Unmarshal([]byte(r.stdout), &summary); err != nil {
		t.Fatalf("%v: %q", err, r.stdout)
	}
	if got := summary.Files[0].Extracted; !slices.Equal(got, []string{filepath.Join(dest, "billing")}) {
		t.Errorf("tar.gz extracted %v", got)
	}
	if got := summary.Files[1].Extracted; !slices.Equal(got, []string{filepath.Join(dest, "docs")}) {
		t.Errorf("zip extracted %v", got)
	}
	if summary.Files[2].Extracted != nil {
		t.Errorf("README.md extracted %v", summary.Files[2].Extracted)
	}
	for name, want := range map[string]string{"billing/bin/billing": "binary", "docs/a.txt": "a", "README.md": "readme"} {
		if got := readFile(t, filepath.Join(dest, name)); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	for _, archive := range []string{"billing.tar.gz", "billing.zip"} {
		if _, err := os.Stat(filepath.Join(dest, archive)); err != nil {
			t.Errorf("the archive is not kept: %v", err)
		}
	}
	if !strings.Contains(r.stderr, "billing.tar.gz  extracted: billing") {
		t.Errorf("stderr = %q", r.stderr)
	}
	noExtractDirs(t, dest)

	write(t, filepath.Join(dest, "docs", "a.txt"), "changed")
	r = download(t, srv.URL, args...)
	if r.err == nil || !strings.Contains(r.err.Error(), "extracting billing.tar.gz would replace billing in") {
		t.Fatalf("err = %v", r.err)
	}
	if readFile(t, filepath.Join(dest, "docs", "a.txt")) != "changed" {
		t.Error("an entry was replaced without --force")
	}
	noExtractDirs(t, dest)

	r = download(t, srv.URL, append(args, "--force")...)
	if r.err != nil || readFile(t, filepath.Join(dest, "docs", "a.txt")) != "a" {
		t.Errorf("--force: %v", r.err)
	}
	noExtractDirs(t, dest)
}

func TestDownloadExtractRefusesUnsafeArchive(t *testing.T) {
	isolate(t)
	srv := newDownloadServer(t)
	srv.add("evil.tar.gz", "archive", "", "", tarGz(t, map[string]string{"ok.txt": "ok", "../evil": "evil"}))
	srv.add("billing.7z", "archive", "", "", "7z")
	parent := t.TempDir()
	dest := filepath.Join(parent, "opt")

	r := download(t, srv.URL, "--file", "evil.tar.gz", "--extract", "-d", dest)
	if r.err == nil || !strings.Contains(r.err.Error(), `refused entry "../evil"`) {
		t.Fatalf("err = %v", r.err)
	}
	for _, path := range []string{filepath.Join(parent, "evil"), filepath.Join(dest, "ok.txt")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s written", path)
		}
	}
	noExtractDirs(t, dest)

	r = download(t, srv.URL, "--file", "billing.7z", "--extract", "-d", dest)
	if r.err == nil || !strings.Contains(r.err.Error(), "billing.7z: unknown archive format") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadExtractDryRun(t *testing.T) {
	isolate(t)
	srv := newArchivesRelease(t)
	dest := filepath.Join(t.TempDir(), "opt")

	r := download(t, srv.URL, "--file", "billing.tar.gz", "--extract", "--dry-run", "-d", dest)
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "billing.tar.gz  extracted into "+dest+" (not done: dry run)") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a dry run created %s", dest)
	}
}

func TestDownloadReleaseDocumentErrors(t *testing.T) {
	isolate(t)
	srv := newDownloadServer(t)
	srv.add("README.md", "file", "", "", "readme")

	for _, tc := range []struct {
		release, version, want string
	}{
		{`not json`, "1.2.0", "the response is not JSON"},
		{`{"artifacts": "README.md"}`, "1.2.0", "read the release.json of billing 1.2.0"},
		{`{"product": "billing", "artifacts": []}`, "latest", "the release.json of billing latest has no version"},
		{`{"product": "billing", "version": "../1.2.0", "artifacts": []}`, "1.2.0", `invalid version "../1.2.0"`},
	} {
		srv.mu.Lock()
		srv.rawRelease = tc.release
		srv.mu.Unlock()
		r := download(t, srv.URL, tc.version, "--all")
		if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.release, r.err, tc.want)
		}
	}

	srv.mu.Lock()
	srv.rawRelease = `{"product": "billing", "artifacts": [{"name": "README.md", "kind": "file", "size": 6, "sha256": "` +
		util.Sha256String("readme") + `"}]}`
	srv.mu.Unlock()
	r := download(t, srv.URL, "1.2.0", "--file", "README.md", "-o", "json")
	if r.err != nil || !strings.Contains(r.stdout, `"version": "1.2.0"`) {
		t.Errorf("a release.json without version, for 1.2.0: %v %q", r.err, r.stdout)
	}
}

func TestDownloadFileErrors(t *testing.T) {
	dir := isolate(t)
	srv := newDownloadServer(t)
	srv.add("refused.txt", "file", "", "", "refused")
	srv.add("closed.txt", "file", "", "", "closed")
	srv.add("cut.txt", "file", "", "", "cut in the middle")
	srv.failures["refused.txt"] = http.StatusInsufficientStorage
	srv.cuts["closed.txt"] = 0
	srv.cuts["cut.txt"] = 4

	for name, want := range map[string]string{
		"refused.txt": "download refused.txt: disk full",
		"closed.txt":  "download closed.txt: ",
		"cut.txt":     "download cut.txt: unexpected EOF",
	} {
		r := download(t, srv.URL, "--file", name)
		if r.err == nil || !strings.Contains(r.err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, r.err, want)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s written: %v", name, err)
		}
		if !strings.Contains(r.stderr, name) || !strings.Contains(r.stderr, "✗") {
			t.Errorf("%s: stderr = %q", name, r.stderr)
		}
	}
	noPartFiles(t, dir)
}

func TestDownloadDestinationErrors(t *testing.T) {
	dir := isolate(t)
	srv := newDownloadServer(t)
	srv.add("README.md", "file", "", "", "readme")
	write(t, filepath.Join(dir, "file"), "")

	r := download(t, srv.URL, "--file", "README.md", "-d", "$LOCALRECTL_UNDEFINED_DIR/opt")
	if r.err == nil || !strings.Contains(r.err.Error(), "the destination directory $LOCALRECTL_UNDEFINED_DIR/opt") {
		t.Errorf("undefined variable: err = %v", r.err)
	}

	r = download(t, srv.URL, "--file", "README.md", "-d", filepath.Join(dir, "file", "opt"))
	if r.err == nil || !strings.Contains(r.err.Error(), "cannot be read") {
		t.Errorf("under a file: err = %v", r.err)
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	readOnly := filepath.Join(dir, "read-only")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	r = download(t, srv.URL, "--file", "README.md", "-d", filepath.Join(readOnly, "opt"))
	if r.err == nil || !strings.Contains(r.err.Error(), "create the destination directory") {
		t.Errorf("read-only parent: err = %v", r.err)
	}
}
