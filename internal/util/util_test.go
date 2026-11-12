package util

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSha256File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a")
	writeFile(t, path, "abc")
	got, err := Sha256File(path)
	if err != nil || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("Sha256File = %s, %v", got, err)
	}
	if got != Sha256String("abc") || got != Sha265([]byte("abc")) {
		t.Error("the sha256 helpers disagree")
	}
	if _, err := Sha256File(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("no error for a missing file")
	}
	if _, err := Sha256File(t.TempDir()); err == nil {
		t.Error("no error for a directory")
	}
	if MD5([]byte("abc")) != "900150983cd24fb0d6963f7d28e17f72" {
		t.Error("MD5")
	}
}

type closer struct {
	io.Reader
	closed bool
}

func (c *closer) Close() error {
	c.closed = true
	return nil
}

func TestProgressReader(t *testing.T) {
	var reports [][2]int64
	src := &closer{Reader: strings.NewReader("hello world")}
	r := NewProgressReader(src, 11, func(done, total int64) { reports = append(reports, [2]int64{done, total}) })

	// Read by 4 bytes: 3 reads, 3 reports.
	var out bytes.Buffer
	buf := make([]byte, 4)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "hello world" {
		t.Errorf("read %q", out.String())
	}
	if len(reports) != 3 || reports[2] != [2]int64{11, 11} {
		t.Errorf("reports = %v", reports)
	}
	if err := r.Close(); err != nil || !src.closed {
		t.Errorf("Close = %v, closed = %v", err, src.closed)
	}

	// Without a callback nor a closer.
	plain := &ProgressReader{R: strings.NewReader("x")}
	if b, err := io.ReadAll(plain); err != nil || string(b) != "x" || plain.Close() != nil {
		t.Errorf("plain reader: %q, %v", b, err)
	}
}

func TestCreateSoftTempdir(t *testing.T) {
	dir, err := CreateSoftTempdir()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || !strings.HasPrefix(filepath.Base(dir), "localrectl-") {
		t.Errorf("%s: %v", dir, err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestArchiveErrors(t *testing.T) {
	src := newTree(t)
	dst := t.TempDir()
	if err := os.Symlink(filepath.Join(src, "amd64", "app"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	for _, compress := range []func(string, string) error{TarGzDir, ZipDir} {
		err := compress(src, filepath.Join(dst, "a"))
		if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Errorf("err = %v, want a link refused", err)
		}
		if err := compress(filepath.Join(dst, "none"), filepath.Join(dst, "b")); err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing source: err = %v", err)
		}
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("a failed archive leaves %v", entries)
	}
}

func TestSingleFileArchives(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "app")
	writeFile(t, src, strings.Repeat("app", 100))
	for name, compress := range map[string]func(string, string) error{"gz": GzipFile, "zip": ZipFile} {
		dst := filepath.Join(dir, "app."+name)
		if err := compress(src, dst); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
			t.Errorf("%s: %v", name, err)
		}
		if err := compress(dir, dst); err == nil {
			t.Errorf("%s: no error for a directory", name)
		}
	}
}
