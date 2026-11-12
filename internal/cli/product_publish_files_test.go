package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
)

func newFilesCmd(t *testing.T, f publishFlags) (*publishCmd, *bytes.Buffer) {
	t.Helper()
	var errb bytes.Buffer
	printer := logging.NewPrinter(io.Discard, &errb, logging.Options{NoColor: true, Verbose: true})
	p := newPublishCmd("billing", printer, &config.Config{}, f)
	t.Cleanup(p.tmpCleanup)
	return p, &errb
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRootDirSkipsHiddenFiles(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "a")
	write(t, filepath.Join(root, ".env"), "TOKEN=x")
	write(t, filepath.Join(root, ".git", "config"), "x")
	p, _ := newFilesCmd(t, publishFlags{})

	files, err := p.readFilesFromRootDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "a.txt" {
		t.Errorf("files = %v", files)
	}
	if p.tmpDir != "" || len(p.tmpPaths) != 0 {
		t.Errorf("a temporary directory is created without --compress-subdirs: %v", p.tmpPaths)
	}
}

func TestRootDirSkipsLinks(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "a")
	secret := write(t, filepath.Join(t.TempDir(), "secret"), "x")
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	p, _ := newFilesCmd(t, publishFlags{})

	files, err := p.readFilesFromRootDir(root)
	if err != nil || len(files) != 1 || filepath.Base(files[0]) != "a.txt" {
		t.Errorf("files = %v, %v", files, err)
	}
}

func TestCompressDirCreatesOneTempDir(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "linux", "app"), "l")
	write(t, filepath.Join(root, "darwin", "app"), "d")
	p, _ := newFilesCmd(t, publishFlags{compressSubdirectories: true, compressSubdirType: "zip"})

	files, err := p.readFilesFromRootDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || len(p.tmpPaths) != 1 || filepath.Dir(files[0]) != p.tmpDir || filepath.Ext(files[1]) != ".zip" {
		t.Errorf("files = %v, temporary paths = %v", files, p.tmpPaths)
	}
	tmp := p.tmpDir
	p.tmpCleanup()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("%s is not removed: %v", tmp, err)
	}
}

// goreleaserBuild writes a goreleaser build under dir: dist/artifacts.json, dist/metadata.json
// and the archive dist/app.tar.gz, listed as artifactPath.
func goreleaserBuild(t *testing.T, dir, artifactPath, metadata string) {
	t.Helper()
	write(t, filepath.Join(dir, "dist", "app.tar.gz"), "app")
	write(t, filepath.Join(dir, "dist", "artifacts.json"),
		`[{"name":"app.tar.gz","path":"`+artifactPath+`","type":"Archive"},{"name":"x","path":"x","type":"Binary"}]`)
	if metadata != "" {
		write(t, filepath.Join(dir, "dist", "metadata.json"), metadata)
	}
}

func TestGoReleaserFilesRelativeToRoot(t *testing.T) {
	dir := t.TempDir()
	goreleaserBuild(t, dir, "dist/app.tar.gz", `{"tag":"v1.2.0","version":"1.2.0"}`)
	t.Chdir(t.TempDir()) // the current directory is not the root: --root is used

	// --goreleaser-artifacts-file without --goreleaser-dir: the paths are relative to --root.
	p, _ := newFilesCmd(t, publishFlags{
		goReleaserArtifactsFile: filepath.Join(dir, "dist", "artifacts.json"),
		goReleaserMetadataFile:  filepath.Join(dir, "dist", "metadata.json"),
	})
	files, versions, err := p.readFilesFromGoReleaser(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	if want := []string{"app.tar.gz", "artifacts.json", "metadata.json"}; !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
	if !slices.Equal(versions, []string{"v1.2.0", "1.2.0"}) {
		t.Errorf("versions = %v, want the tag first", versions)
	}
}

func TestGoReleaserFilesRelativeToDir(t *testing.T) {
	dir := t.TempDir()
	goreleaserBuild(t, dir, "dist/app.tar.gz", "")
	p, _ := newFilesCmd(t, publishFlags{
		goReleaserDir:           filepath.Join(dir, "dist"),
		goReleaserArtifactsFile: filepath.Join(dir, "dist", "artifacts.json"),
	})
	files, versions, err := p.readFilesFromGoReleaser("ignored")
	if err != nil || len(files) != 2 || versions != nil {
		t.Errorf("files = %v, versions = %v, err = %v", files, versions, err)
	}
}

func TestGoReleaserMissingEntry(t *testing.T) {
	dir := t.TempDir()
	goreleaserBuild(t, dir, "dist/missing.tar.gz", "")
	p, _ := newFilesCmd(t, publishFlags{goReleaserArtifactsFile: filepath.Join(dir, "dist", "artifacts.json")})

	_, _, err := p.readFilesFromGoReleaser(dir)
	if err == nil || !strings.Contains(err.Error(), "missing.tar.gz") {
		t.Errorf("err = %v, want the missing entry", err)
	}
}

func TestGoReleaserBadFiles(t *testing.T) {
	dir := t.TempDir()
	p, _ := newFilesCmd(t, publishFlags{goReleaserArtifactsFile: filepath.Join(dir, "none.json")})
	if _, _, err := p.readFilesFromGoReleaser(dir); err == nil || !strings.Contains(err.Error(), "read the goreleaser artifacts") {
		t.Errorf("missing artifacts.json: err = %v", err)
	}

	p.pFlags.goReleaserArtifactsFile = write(t, filepath.Join(dir, "artifacts.json"), "not json")
	if _, _, err := p.readFilesFromGoReleaser(dir); err == nil || !strings.Contains(err.Error(), "is not a valid artifacts.json") {
		t.Errorf("bad artifacts.json: err = %v", err)
	}

	p.pFlags.goReleaserArtifactsFile = write(t, filepath.Join(dir, "artifacts.json"), "[]")
	p.pFlags.goReleaserMetadataFile = write(t, filepath.Join(dir, "metadata.json"), "not json")
	if _, _, err := p.readFilesFromGoReleaser(dir); err == nil || !strings.Contains(err.Error(), "is not a valid metadata.json") {
		t.Errorf("bad metadata.json: err = %v", err)
	}
}

func TestPublishVersionFromMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, version, want, wantErr string
	}{
		{"tag first", `{"tag":"v1.2.0","version":"1.2.0"}`, "", "v1.2.0", ""},
		{"version alone", `{"version":"1.2.0"}`, "", "1.2.0", ""},
		{"--version is the version", `{"tag":"v1.2.0","version":"1.2.0"}`, "1.2.0", "1.2.0", ""},
		{"--version is neither", `{"tag":"v1.2.0","version":"1.2.0"}`, "1.3.0", "", "--version 1.3.0 is not the tag nor the version of metadata.json (v1.2.0, 1.2.0)"},
		{"no version at all", `{}`, "", "", "the version is required"},
		{"bad version", `{"tag":"a/b"}`, "", "", `invalid version "a/b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			goreleaserBuild(t, dir, "dist/app.tar.gz", tc.metadata)
			srv := newRegistryServer(t, http.StatusOK, "")
			p, cmd, _ := newPublishRun(t, srv.URL, token, skip)
			p.pFlags.version = tc.version
			p.pFlags.root = dir
			p.pFlags.goReleaserArtifactsFile = filepath.Join(dir, "dist", "artifacts.json")
			p.pFlags.goReleaserMetadataFile = filepath.Join(dir, "dist", "metadata.json")

			sum, err := p.processPublish(cmd)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || sum.Version != tc.want {
				t.Errorf("summary = %+v, err = %v, want the version %s", sum, err, tc.want)
			}
		})
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := write(t, filepath.Join(dir, "a.yaml"), "x")
	t.Setenv("HOME", dir)

	for _, tc := range []struct {
		path, want string
	}{
		{file, ""},
		{"~/a.yaml", ""},
		{filepath.Join(dir, "none"), "the card file " + filepath.Join(dir, "none") + " does not exist"},
		{dir, "the card file " + dir + " is a directory"},
		{"~other/a", "the card file ~other/a: "},
	} {
		err := fileExists("card", tc.path)
		if tc.want == "" {
			if err != nil {
				t.Errorf("fileExists(%s) = %v", tc.path, err)
			}
			continue
		}
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("fileExists(%s) = %v, want %q", tc.path, err, tc.want)
		}
	}
}

func TestPostValidateRejectsBadRootArtifacts(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "artifacts.json"), "not json")
	log := logging.NewPrinter(io.Discard, io.Discard, logging.Options{}).Logger()
	vCmd := newValidateCmd(&config.Config{}, log)

	f := publishFlags{root: root}
	if err := f.postValidateFlags(log, context.Background(), vCmd); err == nil || !strings.Contains(err.Error(), "the root directory holds an artifacts.json") {
		t.Errorf("err = %v", err)
	}
	if f.goReleaserDir != "" {
		t.Error("switched to a goreleaser build with an artifacts.json that is not valid")
	}

	// --no-validate sends it as it is.
	f = publishFlags{root: root, noValidate: true}
	if err := f.postValidateFlags(log, context.Background(), vCmd); err != nil || f.goReleaserDir != root {
		t.Errorf("--no-validate: err = %v, goreleaser dir = %q", err, f.goReleaserDir)
	}
}

func TestSendsTokenInClear(t *testing.T) {
	for url, want := range map[string]bool{
		"https://registry.example.com": false,
		"http://localhost:8077":        false,
		"http://127.0.0.1:8077":        false,
		"http://[::1]:8077":            false,
		"http://registry.example.com":  true,
		"http://10.0.0.5":              true,
		"::not a url":                  false,
	} {
		if got := sendsTokenInClear(url); got != want {
			t.Errorf("sendsTokenInClear(%s) = %v, want %v", url, got, want)
		}
	}
}

func TestPublishWarnsTokenInClear(t *testing.T) {
	// The test server listens on 127.0.0.1: no warning (sendsTokenInClear checks the others).
	srv := newRegistryServer(t, http.StatusNotFound, "")
	p, cmd, errb := newPublishRun(t, srv.URL, token, fail)
	if _, err := p.processPublish(cmd); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errb.String(), "in clear") {
		t.Errorf("warned for a loopback server: %q", errb.String())
	}
}

func TestPublishNameAndVersionChecked(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"publish", "..", "--product-file", "x"},
		{"publish", "a/b", "--product-file", "x"},
	} {
		r := run(t, args...)
		if r.err == nil || !strings.Contains(r.err.Error(), "invalid product name") {
			t.Errorf("%v: err = %v", args, r.err)
		}
		if !config.IsUsageError(r.err) {
			t.Errorf("%v: not a usage error: %T", args, r.err)
		}
	}
}

func TestPublishRetrySeesItsOwnPublish(t *testing.T) {
	// The first POST answers 503 (its answer lost), the second 409: the version is published.
	srv := newPublishFlowServer(t, []int{http.StatusServiceUnavailable, http.StatusConflict}, http.StatusOK)
	p, _ := newUploadCmd(t, srv.URL, token, 1)
	if err := p.httpPublishCall(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}

	// A 409 at the first attempt is another job: it fails.
	srv = newPublishFlowServer(t, []int{http.StatusConflict}, http.StatusOK)
	p, _ = newUploadCmd(t, srv.URL, token, 1)
	if err := p.httpPublishCall(context.Background()); !errors.Is(err, errAlreadyPublished) {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadSkipDisplay(t *testing.T) {
	srv := newUploadServer(t, "already published", http.StatusConflict)
	p, errb := newUploadCmd(t, srv.URL, token, 0)
	p.pFlags.ifExists = skip

	err := p.httpUpload(context.Background(), "a", "a.tar.gz", writeAsset(t, "x"), "sum")
	if !errors.Is(err, errAlreadyPublished) || !strings.HasSuffix(errb.String(), "–\n") {
		t.Errorf("err = %v, stderr = %q", err, errb.String())
	}
}

// newPublishFlowServer answers the POSTs with posts, in order (the last one repeats), and the GET
// of the release with release.
func newPublishFlowServer(t *testing.T, posts []int, release int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(release)
			return
		}
		mu.Lock()
		status := posts[min(n, len(posts)-1)]
		n++
		mu.Unlock()
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"product":"billing","version":"v1.2.3"}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"busy or published"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}
