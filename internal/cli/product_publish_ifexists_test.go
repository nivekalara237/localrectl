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
	"github.com/spf13/cobra"
)

// registryServer is a fake server for a whole publish: GET of the release, PUT of the files and
// POST publish. release is the status of the GET; conflict is "PUT <name>" or "POST", the
// request answered 409 as if another job had just published the version.
type registryServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newRegistryServer(t *testing.T, release int, conflict string) *registryServer {
	t.Helper()
	s := &registryServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		req := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()

		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(release)
			_, _ = io.WriteString(w, `{"version":"1.2.0"}`)
		case conflict != "" && strings.HasPrefix(req, strings.Replace(conflict, "PUT ", "PUT /billing/artifacts/1.2.0/files/", 1)):
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"version billing/1.2.0 is already published and cannot be changed"}`)
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

// sent returns the PUT and POST requests: what was sent.
func (s *registryServer) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.requests), func(r string) bool { return strings.HasPrefix(r, "GET ") })
}

// newPublishRun returns a publish of a directory holding a.txt and b.txt, with a product card.
func newPublishRun(t *testing.T, url, uploadToken string, ifExists ifExists) (*publishCmd, *cobra.Command, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "out")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"out/a.txt": "a", "out/b.txt": "bb", "card.yaml": "name: billing\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var errb bytes.Buffer
	cfg := &config.Config{AppUntypedConfig: config.AppUntypedConfig{Server: config.ServerConf{BaseUrl: url, UploadToken: uploadToken}}}
	printer := logging.NewPrinter(io.Discard, &errb, logging.Options{NoColor: true})
	p := newPublishCmd("billing", printer, cfg, publishFlags{
		version: "1.2.0", root: root, productFile: filepath.Join(dir, "card.yaml"), ifExists: ifExists,
	})
	t.Cleanup(p.tmpCleanup)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return p, cmd, &errb
}

func TestIfExistsNotPublished(t *testing.T) {
	for _, mode := range []ifExists{"", fail, skip} {
		srv := newRegistryServer(t, http.StatusNotFound, "")
		p, cmd, _ := newPublishRun(t, srv.URL, token, mode)

		sum, err := p.processPublish(cmd)
		if err != nil {
			t.Fatalf("--if-exists %q: %v", mode, err)
		}
		want := []string{
			"PUT /billing/artifacts/1.2.0/files/a.txt", "PUT /billing/artifacts/1.2.0/files/b.txt",
			"PUT /billing/artifacts/1.2.0/files/registry.yaml", "POST /billing/artifacts/1.2.0/publish",
		}
		if !slices.Equal(srv.sent(), want) {
			t.Errorf("--if-exists %q sent %v", mode, srv.sent())
		}
		if sum.Status != "PUBLISHED" || sum.Total != 3 {
			t.Errorf("--if-exists %q: summary %+v", mode, sum)
		}
		if srv.requests[0] != "GET /billing/releases/1.2.0" {
			t.Errorf("first request = %s, want the check", srv.requests[0])
		}
	}
}

func TestIfExistsFail(t *testing.T) {
	for _, mode := range []ifExists{"", fail} {
		srv := newRegistryServer(t, http.StatusOK, "")
		p, cmd, _ := newPublishRun(t, srv.URL, token, mode)

		_, err := p.processPublish(cmd)
		if !errors.Is(err, errAlreadyPublished) || !strings.Contains(err.Error(), "billing 1.2.0: the version is already published: pass --if-exists skip") {
			t.Errorf("--if-exists %q: err = %v", mode, err)
		}
		if len(srv.sent()) != 0 {
			t.Errorf("--if-exists %q sent %v", mode, srv.sent())
		}
	}
}

func TestIfExistsSkip(t *testing.T) {
	srv := newRegistryServer(t, http.StatusOK, "")
	// No token: nothing is sent, so none is needed.
	p, cmd, errb := newPublishRun(t, srv.URL, "", skip)

	sum, err := p.processPublish(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if want := (summary{Total: 0, TotalSize: 0, Status: "SKIPPED", Version: "1.2.0", Product: "billing"}); *sum != want {
		t.Errorf("summary = %+v, want %+v", *sum, want)
	}
	if len(srv.sent()) != 0 {
		t.Errorf("sent %v", srv.sent())
	}
	if !strings.Contains(errb.String(), "--if-exists skip") {
		t.Errorf("no log of the skip: %q", errb.String())
	}
}

func TestIfExistsSkipDryRun(t *testing.T) {
	srv := newRegistryServer(t, http.StatusOK, "")
	p, cmd, _ := newPublishRun(t, srv.URL, "", skip)
	p.pFlags.dryRun = true

	sum, err := p.processPublish(cmd)
	if err != nil || sum.Status != "SKIPPED" {
		t.Fatalf("summary = %+v, err = %v", sum, err)
	}
}

func TestIfExistsFailDryRun(t *testing.T) {
	srv := newRegistryServer(t, http.StatusOK, "")
	p, cmd, _ := newPublishRun(t, srv.URL, "", fail)
	p.pFlags.dryRun = true

	if _, err := p.processPublish(cmd); !errors.Is(err, errAlreadyPublished) {
		t.Fatalf("err = %v", err)
	}
}

func TestIfExistsPublishedMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		conflict string
		sent     int // files sent before the 409
		requests int
	}{
		{"PUT b.txt", 1, 2},
		{"POST", 3, 4},
	} {
		t.Run(tc.conflict, func(t *testing.T) {
			srv := newRegistryServer(t, http.StatusNotFound, tc.conflict)
			p, cmd, _ := newPublishRun(t, srv.URL, token, skip)

			sum, err := p.processPublish(cmd)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Status != "SKIPPED" || sum.Total != tc.sent {
				t.Errorf("summary = %+v, want SKIPPED after %d files", sum, tc.sent)
			}
			if len(srv.sent()) != tc.requests {
				t.Errorf("sent %v: nothing is sent after the 409", srv.sent())
			}

			srv = newRegistryServer(t, http.StatusNotFound, tc.conflict)
			p, cmd, _ = newPublishRun(t, srv.URL, token, fail)
			if _, err := p.processPublish(cmd); !errors.Is(err, errAlreadyPublished) {
				t.Errorf("--if-exists fail: err = %v", err)
			}
		})
	}
}

func TestIfExistsCheckFails(t *testing.T) {
	srv := newRegistryServer(t, http.StatusBadRequest, "")
	p, cmd, _ := newPublishRun(t, srv.URL, token, skip)
	if _, err := p.processPublish(cmd); err == nil || !strings.Contains(err.Error(), "check whether billing 1.2.0 is published: 400") {
		t.Errorf("err = %v", err)
	}
	if len(srv.sent()) != 0 {
		t.Errorf("sent %v without knowing", srv.sent())
	}

	p, cmd, _ = newPublishRun(t, "http://127.0.0.1:1", token, skip)
	if _, err := p.processPublish(cmd); err == nil || !strings.Contains(err.Error(), "check whether billing 1.2.0 is published") {
		t.Errorf("unreachable: err = %v", err)
	}
}
