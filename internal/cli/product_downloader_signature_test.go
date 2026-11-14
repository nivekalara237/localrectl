package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/util"
)

const (
	identityIssuer = "https://token.actions.githubusercontent.com"
	identity       = "https://github.com/acme/billing/.github/workflows/release.yml@refs/tags/v1.2.0"
)

// fakeCosign puts a cosign in PATH that writes its arguments, one per line, to the file it
// returns, and fails with "invalid signature" when exitCode is not 0.
func fakeCosign(t *testing.T, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake cosign is a shell script")
	}
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args")
	script := fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do echo \"$arg\" >> %q; done\n", argsFile)
	if exitCode != 0 {
		script += fmt.Sprintf("echo 'Error: invalid signature' >&2\nexit %d\n", exitCode)
	}
	if err := os.WriteFile(filepath.Join(binDir, "cosign"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return argsFile
}

func cosignArgsOf(t *testing.T, argsFile string) []string {
	t.Helper()
	content, err := os.ReadFile(argsFile)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

// newSignedRelease serves README.md and LICENSE, with a checksums file that lists them, signed
// by signatureName, with a certificate when certificateName is set.
func newSignedRelease(t *testing.T, signatureName, certificateName string) *downloadServer {
	t.Helper()
	srv := newDownloadServer(t)
	srv.add("README.md", "file", "", "", "readme")
	srv.add("LICENSE", "file", "", "", "license")
	srv.contents["checksums.txt"] = util.Sha256String("readme") + "  README.md\n" + util.Sha256String("license") + " *LICENSE\n"
	srv.contents[signatureName] = "signature"
	srv.checksums["signature"] = srv.URL + "/billing/artifacts/1.2.0/files/" + signatureName
	if certificateName != "" {
		srv.contents[certificateName] = "certificate"
		srv.checksums["certificate"] = srv.URL + "/billing/artifacts/1.2.0/files/" + certificateName
	}
	return srv
}

func keyless() []string {
	return []string{"--verify-signature", "--certificate-identity", identity, "--certificate-oidc-issuer", identityIssuer}
}

func TestDownloadVerifySignatureKeyless(t *testing.T) {
	isolate(t)
	argsFile := fakeCosign(t, 0)
	srv := newSignedRelease(t, "checksums.txt.sig", "checksums.txt.pem")
	dest := t.TempDir()

	r := download(t, srv.URL, append(keyless(), "--file", "README.md", "-d", dest, "-o", "json")...)
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	got := cosignArgsOf(t, argsFile)
	if len(got) != 10 || got[0] != "verify-blob" || got[1] != "--signature" || !strings.HasSuffix(got[2], "checksums.txt.sig") ||
		got[3] != "--certificate" || !strings.HasSuffix(got[4], "checksums.txt.pem") ||
		!slices.Equal(got[5:9], []string{"--certificate-identity", identity, "--certificate-oidc-issuer", identityIssuer}) ||
		!strings.HasSuffix(got[9], "checksums.txt") {
		t.Errorf("cosign args = %q", got)
	}
	if _, err := os.Stat(filepath.Dir(got[2])); !os.IsNotExist(err) {
		t.Errorf("the signature directory is kept: %v", err)
	}

	var summary downloadSummary
	if err := json.Unmarshal([]byte(r.stdout), &summary); err != nil {
		t.Fatalf("%v: %q", err, r.stdout)
	}
	if !summary.SignatureVerified || len(summary.Files) != 1 || !summary.Files[0].Verified {
		t.Errorf("summary = %+v", summary)
	}
	if !strings.Contains(r.stderr, "checksums.txt  signature verified (cosign)") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if got := readFile(t, filepath.Join(dest, "README.md")); got != "readme" {
		t.Errorf("README.md = %q", got)
	}
}

func TestDownloadVerifySignatureBundleWithKey(t *testing.T) {
	dir := isolate(t)
	argsFile := fakeCosign(t, 0)
	srv := newSignedRelease(t, "checksums.txt.sigstore.json", "")
	key := write(t, filepath.Join(dir, "cosign.pub"), "key")

	r := download(t, srv.URL, "--verify-signature", "--key", key, "--all")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	got := cosignArgsOf(t, argsFile)
	if len(got) != 6 || got[1] != "--bundle" || !strings.HasSuffix(got[2], "checksums.txt.sigstore.json") || got[3] != "--key" || got[4] != key {
		t.Errorf("cosign args = %q", got)
	}

	r = download(t, srv.URL, "--verify-signature", "--certificate-identity-regexp", "^https://github.com/acme/",
		"--certificate-oidc-issuer", identityIssuer, "--file", "LICENSE", "--force")
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	if got := cosignArgsOf(t, argsFile); !slices.Contains(got, "--certificate-identity-regexp") || slices.Contains(got, "--certificate") {
		t.Errorf("a bundle with an identity: cosign args = %q", got)
	}
}

func TestDownloadVerifySignatureRefused(t *testing.T) {
	dir := isolate(t)
	fakeCosign(t, 1)
	srv := newSignedRelease(t, "checksums.txt.sig", "checksums.txt.pem")

	r := download(t, srv.URL, append(keyless(), "--file", "README.md")...)
	if r.err == nil || !strings.Contains(r.err.Error(), "the signature of checksums.txt is not valid: Error: invalid signature") {
		t.Fatalf("err = %v", r.err)
	}
	if slices.Contains(srv.fileRequests(), "/billing/artifacts/1.2.0/files/README.md") {
		t.Error("a file was downloaded before the signature was checked")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); !os.IsNotExist(err) {
		t.Errorf("README.md written: %v", err)
	}
}

func TestDownloadVerifySignatureChecksumsErrors(t *testing.T) {
	isolate(t)
	fakeCosign(t, 0)

	srv := newSignedRelease(t, "checksums.txt.sig", "checksums.txt.pem")
	srv.add("NOTICE", "file", "", "", "notice")
	r := download(t, srv.URL, append(keyless(), "--file", "NOTICE")...)
	if r.err == nil || !strings.Contains(r.err.Error(), "NOTICE is not in the signed checksums file checksums.txt") {
		t.Errorf("err = %v", r.err)
	}

	srv.contents["checksums.txt"] = util.Sha256String("other") + "  README.md\n"
	r = download(t, srv.URL, append(keyless(), "--file", "README.md")...)
	if r.err == nil || !strings.Contains(r.err.Error(), "give different sha256 for README.md") {
		t.Errorf("err = %v", r.err)
	}

	srv.contents["checksums.txt"] = "not a checksums file\n"
	r = download(t, srv.URL, append(keyless(), "--file", "README.md")...)
	if r.err == nil || !strings.Contains(r.err.Error(), "the signed checksums file checksums.txt: line 1") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadVerifySignatureReleaseErrors(t *testing.T) {
	isolate(t)
	fakeCosign(t, 0)

	srv := newSignedRelease(t, "checksums.txt.sig", "")
	for label, tc := range map[string]struct {
		checksums map[string]any
		want      string
	}{
		"no signature":      {map[string]any{"name": "checksums.txt"}, "has no signed checksums file"},
		"no certificate":    {map[string]any{"name": "checksums.txt", "signature": srv.URL + "/f/checksums.txt.sig"}, "has no certificate: pass --key"},
		"foreign signature": {map[string]any{"name": "checksums.txt", "signature": srv.URL + "/f/other.sig"}, `invalid attachment "other.sig"`},
		"hidden signature":  {map[string]any{"name": "checksums.txt", "signature": srv.URL + "/f/%2E%2E%2Fchecksums.txt.sig"}, "invalid attachment"},
		"missing file":      {map[string]any{"name": "checksums.txt", "signature": srv.URL + "/f/checksums.txt.bundle"}, "download checksums.txt.bundle: not found"},
	} {
		srv.mu.Lock()
		srv.checksums = tc.checksums
		srv.mu.Unlock()
		r := download(t, srv.URL, append(keyless(), "--file", "README.md")...)
		if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", label, r.err, tc.want)
		}
	}
}

func TestDownloadVerifySignatureWithoutCosign(t *testing.T) {
	isolate(t)
	t.Setenv("PATH", t.TempDir())
	srv := newSignedRelease(t, "checksums.txt.sig", "checksums.txt.pem")

	r := download(t, srv.URL, append(keyless(), "--file", "README.md")...)
	if r.err == nil || !strings.Contains(r.err.Error(), "needs cosign, which is not installed") {
		t.Errorf("err = %v", r.err)
	}
}

func TestDownloadVerifySignatureDryRun(t *testing.T) {
	isolate(t)
	argsFile := fakeCosign(t, 0)
	srv := newSignedRelease(t, "checksums.txt.sig", "checksums.txt.pem")

	r := download(t, srv.URL, append(keyless(), "--all", "--dry-run", "-o", "json")...)
	if r.err != nil {
		t.Fatalf("%v\nstderr: %s", r.err, r.stderr)
	}
	if cosignArgsOf(t, argsFile) != nil || len(srv.fileRequests()) != 0 {
		t.Errorf("a dry run ran cosign or downloaded %v", srv.fileRequests())
	}
	if !strings.Contains(r.stdout, `"signature_verified": false`) || !strings.Contains(r.stderr, "signature not verified (dry run)") {
		t.Errorf("stdout = %q\nstderr = %q", r.stdout, r.stderr)
	}
}

func TestDownloadSignatureUsageErrors(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"--verify-signature"},
		{"--verify-signature", "--certificate-identity", identity},
		{"--verify-signature", "--certificate-oidc-issuer", identityIssuer},
		{"--verify-signature", "--certificate-identity", identity, "--certificate-identity-regexp", ".*", "--certificate-oidc-issuer", identityIssuer},
		{"--verify-signature", "--key", "k.pub", "--certificate-identity", identity},
		{"--verify-signature", "--no-verify", "--key", "k.pub"},
		{"--key", "k.pub"},
		{"--certificate-oidc-issuer", identityIssuer},
	} {
		r := run(t, append([]string{"download", "billing", "--server-url", "http://127.0.0.1:1"}, args...)...)
		if !config.IsUsageError(r.err) {
			t.Errorf("%v: err = %v, want a usage error", args, r.err)
		}
	}

	r := run(t, "download", "billing", "--server-url", "http://127.0.0.1:1", "--verify-signature", "--key", "missing.pub")
	if r.err == nil || !strings.Contains(r.err.Error(), "the public key file missing.pub does not exist") {
		t.Errorf("err = %v", r.err)
	}
}

func TestParseChecksums(t *testing.T) {
	sum := util.Sha256String("a")
	path := write(t, filepath.Join(t.TempDir(), "sums"), strings.ToUpper(sum)+"  a.tar.gz\n\n"+sum+" *b.zip\n")
	sums, err := parseChecksums(path)
	if err != nil || sums["a.tar.gz"] != sum || sums["b.zip"] != sum || len(sums) != 2 {
		t.Errorf("sums = %v, %v", sums, err)
	}

	for _, content := range []string{"abc  a\n", sum + "\n", sum + "  a b\n"} {
		path := write(t, filepath.Join(t.TempDir(), "sums"), content)
		if _, err := parseChecksums(path); err == nil {
			t.Errorf("%q accepted", content)
		}
	}
	if _, err := parseChecksums(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file read")
	}
}

func TestAttachmentName(t *testing.T) {
	for rawURL, want := range map[string]string{
		"https://r.example.com/billing/artifacts/1.2.0/files/checksums.txt.sig":           "checksums.txt.sig",
		"https://r.example.com/billing/artifacts/1.2.0/files/checksums.txt.sigstore.json": "checksums.txt.sigstore.json",
		"https://r.example.com/billing/artifacts/1.2.0/files/checksums%20v2.txt.pem":      "checksums v2.txt.pem",
		"/billing/artifacts/1.2.0/files/checksums.txt.bundle":                             "checksums.txt.bundle",
	} {
		covered := "checksums.txt"
		if strings.Contains(rawURL, "%20") {
			covered = "checksums v2.txt"
		}
		if got, err := attachmentName(rawURL, covered); err != nil || got != want {
			t.Errorf("%s: %q, %v, want %q", rawURL, got, err, want)
		}
	}

	for rawURL, want := range map[string]string{
		"https://r.example.com/%zz":                    "invalid attachment URL",
		"https://r.example.com/files/checksums.txt":    "invalid attachment",
		"https://r.example.com/files/other.sig":        "invalid attachment",
		"https://r.example.com/files/checksums.txt%2F": "invalid attachment",
		"https://r.example.com/files/":                 "invalid attachment",
	} {
		if _, err := attachmentName(rawURL, "checksums.txt"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", rawURL, err, want)
		}
	}
}
