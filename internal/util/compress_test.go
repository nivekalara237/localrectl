package util

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func newTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "linux")
	for _, name := range []string{"amd64/app", "arm64/app", ".env", "amd64/.DS_Store", ".git/config"} {
		writeFile(t, filepath.Join(dir, name), name)
	}
	return dir
}

func tarEntries(t *testing.T, path string) ([]string, []*tar.Header) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	var headers []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, headers
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		headers = append(headers, h)
	}
}

func zipEntries(t *testing.T, path string) ([]string, []*zip.FileHeader) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	var headers []*zip.FileHeader
	for _, f := range r.File {
		names = append(names, f.Name)
		headers = append(headers, &f.FileHeader)
	}
	return names, headers
}

func TestArchivesSkipHiddenFiles(t *testing.T) {
	src := newTree(t)
	dst := t.TempDir()

	if err := TarGzDir(src, filepath.Join(dst, "a.tar.gz")); err != nil {
		t.Fatal(err)
	}
	names, _ := tarEntries(t, filepath.Join(dst, "a.tar.gz"))
	if want := []string{"./", "amd64/", "amd64/app", "arm64/", "arm64/app"}; !slices.Equal(names, want) {
		t.Errorf("tar.gz entries = %v, want %v", names, want)
	}

	if err := ZipDir(src, filepath.Join(dst, "a.zip")); err != nil {
		t.Fatal(err)
	}
	names, _ = zipEntries(t, filepath.Join(dst, "a.zip"))
	if want := []string{"amd64/", "amd64/app", "arm64/", "arm64/app"}; !slices.Equal(names, want) {
		t.Errorf("zip entries = %v, want %v", names, want)
	}
}

func TestArchivesWithoutOwner(t *testing.T) {
	src := newTree(t)
	dst := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := TarGzDir(src, dst); err != nil {
		t.Fatal(err)
	}
	_, headers := tarEntries(t, dst)
	for _, h := range headers {
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s keeps its owner: %d:%d %s:%s", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
	}
}

func TestArchivesSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	epoch := time.Unix(1700000000, 0)
	src := newTree(t)
	dst := t.TempDir()

	if err := TarGzDir(src, filepath.Join(dst, "a.tar.gz")); err != nil {
		t.Fatal(err)
	}
	_, theaders := tarEntries(t, filepath.Join(dst, "a.tar.gz"))
	for _, h := range theaders {
		if !h.ModTime.Equal(epoch) {
			t.Errorf("tar %s: ModTime = %v", h.Name, h.ModTime)
		}
	}

	if err := ZipDir(src, filepath.Join(dst, "a.zip")); err != nil {
		t.Fatal(err)
	}
	_, zheaders := zipEntries(t, filepath.Join(dst, "a.zip"))
	for _, h := range zheaders {
		if !h.Modified.Equal(epoch) {
			t.Errorf("zip %s: Modified = %v", h.Name, h.Modified)
		}
	}
}

func TestArchivesReproducible(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	sum := func(path string) [32]byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	// Two copies of the same files, written at different times.
	a := newTree(t)
	time.Sleep(1100 * time.Millisecond)
	b := newTree(t)
	dst := t.TempDir()
	for _, tc := range []struct {
		name     string
		compress func(string, string) error
	}{{"tar.gz", TarGzDir}, {"zip", ZipDir}} {
		if err := tc.compress(a, filepath.Join(dst, "a."+tc.name)); err != nil {
			t.Fatal(err)
		}
		if err := tc.compress(b, filepath.Join(dst, "b."+tc.name)); err != nil {
			t.Fatal(err)
		}
		if sum(filepath.Join(dst, "a."+tc.name)) != sum(filepath.Join(dst, "b."+tc.name)) {
			t.Errorf("%s: two copies of the same files give two archives", tc.name)
		}
	}
}

func TestSourceDateEpochInvalid(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "yesterday")
	if _, ok := sourceDateEpoch(); ok {
		t.Error("an invalid SOURCE_DATE_EPOCH is used")
	}
}
