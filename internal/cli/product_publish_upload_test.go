package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/nivekalara237/localrectl/internal/util"
)

type uploadRequest struct {
	method, path, auth, checksum, contentType string
	length                                    int64
	body                                      []byte
}

// uploadServer answers each PUT with the next status of statuses (the last one repeats) and the
// body {"error": msg} when the status is not 201.
type uploadServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []uploadRequest
}

func newUploadServer(t *testing.T, msg string, statuses ...int) *uploadServer {
	t.Helper()
	s := &uploadServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, uploadRequest{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			checksum: r.Header.Get("X-Checksum-Sha256"), contentType: r.Header.Get("Content-Type"),
			length: r.ContentLength, body: body,
		})
		status := statuses[min(len(s.requests), len(statuses))-1]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusCreated {
			_, _ = io.WriteString(w, `{"name":"a.tar.gz"}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"`+msg+`"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func newUploadCmd(t *testing.T, url, uploadToken string, retries int) (*publishCmd, *bytes.Buffer) {
	t.Helper()
	var errb bytes.Buffer
	cfg := &config.Config{AppUntypedConfig: config.AppUntypedConfig{
		Server:  config.ServerConf{BaseUrl: url, UploadToken: uploadToken},
		Retries: retries,
	}}
	printer := logging.NewPrinter(io.Discard, &errb, logging.Options{NoColor: true})
	return newPublishCmd("billing", printer, cfg, publishFlags{version: "v1.2.3"}), &errb
}

func writeAsset(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPutUploads(t *testing.T) {
	srv := newUploadServer(t, "", http.StatusCreated)
	p, errb := newUploadCmd(t, srv.URL, token, 0)
	path := writeAsset(t, "the content")
	sum, _ := util.Sha256File(path)

	if err := p.put(context.Background(), 2, 5, "a.tar.gz", path); err != nil {
		t.Fatal(err)
	}

	if len(srv.requests) != 1 {
		t.Fatalf("%d requests, want 1", len(srv.requests))
	}
	got := srv.requests[0]
	want := uploadRequest{
		method: http.MethodPut, path: "/billing/artifacts/v1.2.3/files/a.tar.gz", auth: "Bearer " + token,
		checksum: sum, contentType: "application/octet-stream", length: 11, body: []byte("the content"),
	}
	if got.method != want.method || got.path != want.path || got.auth != want.auth || got.checksum != want.checksum ||
		got.contentType != want.contentType || got.length != want.length || !bytes.Equal(got.body, want.body) {
		t.Errorf("request = %+v\nwant      %+v", got, want)
	}
	// stderr is not a terminal: one final line, without \r nor color.
	if line := errb.String(); line != "[2/5] a.tar.gz  11 B  ✓\n" {
		t.Errorf("stderr = %q", line)
	}
}

func TestPutDryRun(t *testing.T) {
	srv := newUploadServer(t, "", http.StatusCreated)
	p, errb := newUploadCmd(t, srv.URL, "", 0)
	p.pFlags.dryRun = true

	if err := p.put(context.Background(), 1, 1, "a.tar.gz", writeAsset(t, "x")); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 0 {
		t.Errorf("dry run sent %d requests", len(srv.requests))
	}
	if line := errb.String(); !strings.HasPrefix(line, "[1/1] a.tar.gz  1 B  sha256 ") || !strings.HasSuffix(line, " (not sent: dry run)\n") {
		t.Errorf("stderr = %q", line)
	}
	if p.totalSize != 1 {
		t.Errorf("totalSize = %d, want the size that would be sent: 1", p.totalSize)
	}
}

func TestPutMissingFile(t *testing.T) {
	p, _ := newUploadCmd(t, "http://127.0.0.1:1", token, 0)
	if err := p.put(context.Background(), 1, 1, "none", filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("no error for a missing file")
	}
}

func TestUploadWithoutToken(t *testing.T) {
	srv := newUploadServer(t, "", http.StatusCreated)
	p, _ := newUploadCmd(t, srv.URL, "", 0)

	err := p.httpUpload(context.Background(), "a", "a.tar.gz", writeAsset(t, "x"), "sum")
	if err == nil || !strings.Contains(err.Error(), "LOCAL_REGISTRY_UPLOAD_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if len(srv.requests) != 0 {
		t.Error("a request was sent without a token")
	}
}

func TestUploadRefused(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "the upload token is missing or invalid: invalid or missing bearer token"},
		{http.StatusForbidden, "refused by the server: invalid or missing bearer token"},
		{http.StatusConflict, "the version is already published: invalid or missing bearer token"},
		{http.StatusUnprocessableEntity, "upload a.tar.gz: invalid or missing bearer token"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := newUploadServer(t, "invalid or missing bearer token", tc.status)
			p, errb := newUploadCmd(t, srv.URL, token, 0)

			err := p.httpUpload(context.Background(), "a", "a.tar.gz", writeAsset(t, "x"), "sum")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(errb.String(), token) {
				t.Error("the token is printed")
			}
			if !strings.HasSuffix(errb.String(), "✗\n") {
				t.Errorf("stderr = %q, want a failed line", errb.String())
			}
		})
	}
}

func TestUploadRetriesTheWholeFile(t *testing.T) {
	srv := newUploadServer(t, "busy", http.StatusServiceUnavailable, http.StatusCreated)
	p, _ := newUploadCmd(t, srv.URL, token, 1)

	if err := p.httpUpload(context.Background(), "a", "a.tar.gz", writeAsset(t, "the content"), "sum"); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 2 {
		t.Fatalf("%d requests, want 2", len(srv.requests))
	}
	for i, r := range srv.requests {
		if string(r.body) != "the content" || r.length != 11 {
			t.Errorf("attempt %d sent %q (%d bytes)", i+1, r.body, r.length)
		}
	}
}

func TestUploadUnreachable(t *testing.T) {
	p, errb := newUploadCmd(t, "http://127.0.0.1:1", token, 0)
	err := p.httpUpload(context.Background(), "a", "a.tar.gz", writeAsset(t, "x"), "sum")
	if err == nil || !strings.Contains(err.Error(), "upload a.tar.gz") {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasSuffix(errb.String(), "✗\n") {
		t.Errorf("stderr = %q", errb.String())
	}
}

type publishRequest struct {
	method, path, latest, auth string
}

// newPublishServer answers each POST with the next status of statuses (the last one repeats):
// the published version for 200, {"error": msg} otherwise.
func newPublishServer(t *testing.T, msg string, statuses ...int) (*httptest.Server, *[]publishRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []publishRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, publishRequest{r.Method, r.URL.Path, r.URL.Query().Get("latest"), r.Header.Get("Authorization")})
		status := statuses[min(len(requests), len(statuses))-1]
		mu.Unlock()
		w.WriteHeader(status)
		switch {
		case status != http.StatusOK:
			_, _ = io.WriteString(w, `{"error":"`+msg+`"}`)
		case msg == "bad body":
			_, _ = io.WriteString(w, `not json`)
		default:
			_, _ = io.WriteString(w, `{"product":"billing","version":"v1.2.3","files":["a.tar.gz"],"latest":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestPublishCall(t *testing.T) {
	for _, latest := range []bool{false, true} {
		srv, requests := newPublishServer(t, "", http.StatusOK)
		p, _ := newUploadCmd(t, srv.URL, token, 0)
		p.pFlags.latest = latest

		if err := p.publish(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := publishRequest{http.MethodPost, "/billing/artifacts/v1.2.3/publish", strconv.FormatBool(latest), "Bearer " + token}
		if len(*requests) != 1 || (*requests)[0] != want {
			t.Errorf("requests = %+v, want %+v", *requests, want)
		}
	}
}

func TestPublishDryRun(t *testing.T) {
	srv, requests := newPublishServer(t, "", http.StatusOK)
	p, _ := newUploadCmd(t, srv.URL, "", 0)
	p.pFlags.dryRun = true

	if err := p.publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 0 {
		t.Errorf("dry run sent %d requests", len(*requests))
	}
}

func TestPublishCallWithoutToken(t *testing.T) {
	srv, requests := newPublishServer(t, "", http.StatusOK)
	p, _ := newUploadCmd(t, srv.URL, "", 0)

	if err := p.httpPublishCall(context.Background()); err == nil || !strings.Contains(err.Error(), "no upload token") {
		t.Fatalf("err = %v", err)
	}
	if len(*requests) != 0 {
		t.Error("a request was sent without a token")
	}
}

func TestPublishCallRefused(t *testing.T) {
	for _, tc := range []struct {
		status    int
		msg, want string
	}{
		{http.StatusUnauthorized, "invalid or missing bearer token", "publish billing v1.2.3: the upload token is missing or invalid: invalid or missing bearer token"},
		{http.StatusNotFound, "nothing uploaded for billing/v1.2.3", "publish billing v1.2.3: nothing uploaded for billing/v1.2.3"},
		{http.StatusConflict, "version billing/v1.2.3 is already published", "publish billing v1.2.3: the version is already published: version billing/v1.2.3 is already published"},
		{http.StatusUnprocessableEntity, "a.tar.gz: sha256 mismatch", "publish billing v1.2.3: a.tar.gz: sha256 mismatch"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv, _ := newPublishServer(t, tc.msg, tc.status)
			p, _ := newUploadCmd(t, srv.URL, token, 0)

			err := p.httpPublishCall(context.Background())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v\nwant  %s", err, tc.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Error("the token is printed")
			}
		})
	}
}

func TestPublishCallRetries(t *testing.T) {
	srv, requests := newPublishServer(t, "busy", http.StatusServiceUnavailable, http.StatusOK)
	p, _ := newUploadCmd(t, srv.URL, token, 1)

	if err := p.httpPublishCall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 {
		t.Errorf("%d requests, want 2", len(*requests))
	}
}

func TestPublishCallBadBody(t *testing.T) {
	srv, _ := newPublishServer(t, "bad body", http.StatusOK)
	p, _ := newUploadCmd(t, srv.URL, token, 0)

	if err := p.httpPublishCall(context.Background()); err == nil || !strings.Contains(err.Error(), "not a published version") {
		t.Fatalf("err = %v", err)
	}
}

func TestPublishCallUnreachable(t *testing.T) {
	p, _ := newUploadCmd(t, "http://127.0.0.1:1", token, 0)
	if err := p.httpPublishCall(context.Background()); err == nil || !strings.Contains(err.Error(), "publish billing v1.2.3") {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadCountsOnlySentFiles(t *testing.T) {
	srv := newUploadServer(t, "refused", http.StatusCreated, http.StatusCreated, http.StatusConflict)
	p, _ := newUploadCmd(t, srv.URL, token, 0)

	for _, content := range []string{"12345", "1234567890", "refused"} {
		_ = p.httpUpload(context.Background(), "a", "a.tar.gz", writeAsset(t, content), "sum")
	}
	if p.totalSize != 15 {
		t.Errorf("totalSize = %d, want 15: a refused file is not counted", p.totalSize)
	}
}

func TestPutUsesTheGivenName(t *testing.T) {
	srv := newUploadServer(t, "", http.StatusCreated)
	p, errb := newUploadCmd(t, srv.URL, token, 0)
	card := filepath.Join(t.TempDir(), "card.yaml")
	if err := os.WriteFile(card, []byte("name: billing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.put(context.Background(), 5, 5, "registry.yaml", card); err != nil {
		t.Fatal(err)
	}
	if got := srv.requests[0].path; got != "/billing/artifacts/v1.2.3/files/registry.yaml" {
		t.Errorf("path = %s", got)
	}
	if !strings.HasPrefix(errb.String(), "[5/5] registry.yaml ") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestUploadsOrderAndNames(t *testing.T) {
	p, _ := newUploadCmd(t, "http://127.0.0.1:1", token, 0)
	files := []string{"/out/a.tar.gz", "/out/CHANGELOG.md", "/out/registry.yaml", "/out/b.zip"}

	got, err := p.uploads(files, "/src/card.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Without --changelog-file, the CHANGELOG.md of the directory is sent as any other file.
	want := []upload{{"a.tar.gz", "/out/a.tar.gz"}, {"CHANGELOG.md", "/out/CHANGELOG.md"}, {"b.zip", "/out/b.zip"},
		{"registry.yaml", "/src/card.yaml"}}
	if !slices.Equal(got, want) {
		t.Errorf("uploads = %v\nwant      %v", got, want)
	}

	p.pFlags.changelog = "/src/NOTES.md"
	got, err = p.uploads(files, "/src/card.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want = []upload{{"a.tar.gz", "/out/a.tar.gz"}, {"b.zip", "/out/b.zip"},
		{"registry.yaml", "/src/card.yaml"}, {"CHANGELOG.md", "/src/NOTES.md"}}
	if !slices.Equal(got, want) {
		t.Errorf("uploads = %v\nwant      %v", got, want)
	}
}

func TestUploadsChangelogExpanded(t *testing.T) {
	p, _ := newUploadCmd(t, "http://127.0.0.1:1", token, 0)
	t.Setenv("HOME", "/home/me")

	p.pFlags.changelog = "~/NOTES.md"
	got, err := p.uploads(nil, "card.yaml")
	if err != nil || got[len(got)-1] != (upload{"CHANGELOG.md", "/home/me/NOTES.md"}) {
		t.Errorf("uploads = %v, %v", got, err)
	}

	p.pFlags.changelog = "~other/NOTES.md"
	if _, err := p.uploads(nil, "card.yaml"); err == nil || !strings.Contains(err.Error(), "--changelog-file") {
		t.Errorf("err = %v", err)
	}
}

func TestNoValidateSkipsTheChecks(t *testing.T) {
	// A nil validateCmd panics if it is called: --no-validate must not call it.
	f := publishFlags{noValidate: true, productFile: "missing.yaml"}
	if err := f.validateProductCard(context.Background(), nil); err != nil {
		t.Errorf("product card: %v", err)
	}
	if err := f.validateGoReleaseFile(context.Background(), nil, "missing.json"); err != nil {
		t.Errorf("artifacts.json: %v", err)
	}
}

func TestNoValidateWarns(t *testing.T) {
	isolate(t)
	card := writeAsset(t, "name: billing\n")
	var errb bytes.Buffer
	log := logging.NewPrinter(io.Discard, &errb, logging.Options{NoColor: true}).Logger()
	f := publishFlags{noValidate: true, productFile: card, version: "1.2.0", parallel: 1, compressSubdirType: "tar.gz"}

	// No server: the checks would fail to fetch the rules.
	if err := f.validateFlags("billing", log, context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "--no-validate") {
		t.Errorf("no warning: %q", errb.String())
	}
}
