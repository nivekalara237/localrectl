package util

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// archiveEntry is one entry of a test archive: a directory when its name ends with /, a link
// when link is set.
type archiveEntry struct {
	name, content, link string
	mode                int64
	hardLink            bool
}

// tarGzBytes returns a gzip-compressed tar of entries.
func tarGzBytes(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content)), Typeflag: tar.TypeReg}
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		switch {
		case strings.HasSuffix(entry.name, "/"):
			header.Typeflag, header.Size = tar.TypeDir, 0
		case entry.hardLink:
			header.Typeflag, header.Linkname, header.Size = tar.TypeLink, entry.link, 0
		case entry.link != "":
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, entry.link, 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write([]byte(entry.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// zipBytes returns a zip of entries.
func zipBytes(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := fs.FileMode(entry.mode)
		if mode == 0 {
			mode = 0o644
		}
		switch {
		case strings.HasSuffix(entry.name, "/"):
			mode |= fs.ModeDir
		case entry.link != "":
			mode, entry.content = fs.ModeSymlink|0o777, entry.link
		}
		header.SetMode(mode)
		content, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := content.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeArchive(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type extractFunc func(src, dstDir string, limit int64) error

// formats returns the extraction of each format, with the archive of entries it reads.
func formats(t *testing.T, entries ...archiveEntry) map[string]func() (extractFunc, string) {
	return map[string]func() (extractFunc, string){
		"tar.gz": func() (extractFunc, string) {
			return ExtractTarGz, writeArchive(t, "a.tar.gz", tarGzBytes(t, entries...))
		},
		"zip": func() (extractFunc, string) {
			return ExtractZip, writeArchive(t, "a.zip", zipBytes(t, entries...))
		},
	}
}

func TestExtract(t *testing.T) {
	entries := []archiveEntry{
		{name: "billing/"},
		{name: "billing/bin/billing", content: "binary", mode: 0o755},
		{name: "billing/README.md", content: "readme", mode: 0o600},
		{name: "LICENSE", content: "license"},
	}
	for format, archive := range formats(t, entries...) {
		t.Run(format, func(t *testing.T) {
			extract, src := archive()
			dstDir := t.TempDir()
			if err := extract(src, dstDir, MaxExtractSize); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"billing/bin/billing": "binary", "billing/README.md": "readme", "LICENSE": "license"} {
				got, err := os.ReadFile(filepath.Join(dstDir, name))
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v", name, got, err)
				}
			}
			if runtime.GOOS == "windows" {
				return
			}
			for name, want := range map[string]fs.FileMode{"billing/bin/billing": 0o755, "billing/README.md": 0o644} {
				info, err := os.Stat(filepath.Join(dstDir, name))
				if err != nil || info.Mode().Perm() != want {
					t.Errorf("%s mode = %v, want %v (%v)", name, info.Mode().Perm(), want, err)
				}
			}
		})
	}
}

func TestExtractRefusesUnsafeEntries(t *testing.T) {
	for label, entry := range map[string]archiveEntry{
		"parent":        {name: "../evil", content: "x"},
		"nested parent": {name: "a/../../evil", content: "x"},
		"absolute":      {name: "/tmp/evil", content: "x"},
		"backslash":     {name: `..\evil`, content: "x"},
		"symlink":       {name: "link", link: "/etc/passwd"},
	} {
		for format, archive := range formats(t, entry) {
			t.Run(label+" "+format, func(t *testing.T) {
				extract, src := archive()
				parent := t.TempDir()
				dstDir := filepath.Join(parent, "dst")
				if err := os.Mkdir(dstDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := extract(src, dstDir, MaxExtractSize); err == nil {
					t.Fatal("unsafe entry accepted")
				}
				for _, path := range []string{filepath.Join(parent, "evil"), filepath.Join(dstDir, "link")} {
					if _, err := os.Lstat(path); err == nil {
						t.Errorf("%s written", path)
					}
				}
			})
		}
	}
}

func TestExtractTarRefusesHardLink(t *testing.T) {
	src := writeArchive(t, "a.tar.gz", tarGzBytes(t, archiveEntry{name: "a", content: "a"}, archiveEntry{name: "b", link: "a", hardLink: true}))
	if err := ExtractTarGz(src, t.TempDir(), MaxExtractSize); err == nil || !strings.Contains(err.Error(), "b is not a regular file") {
		t.Errorf("err = %v", err)
	}
}

func TestExtractLimit(t *testing.T) {
	entries := []archiveEntry{{name: "a", content: "12345"}, {name: "b", content: "67890"}}
	for format, archive := range formats(t, entries...) {
		t.Run(format, func(t *testing.T) {
			extract, src := archive()
			if err := extract(src, t.TempDir(), 10); err != nil {
				t.Errorf("10 bytes within a limit of 10: %v", err)
			}
			if err := extract(src, t.TempDir(), 9); err == nil || !strings.Contains(err.Error(), "exceed 9 bytes") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestExtractDuplicateEntry(t *testing.T) {
	entries := []archiveEntry{{name: "a", content: "first"}, {name: "a", content: "second"}}
	for format, archive := range formats(t, entries...) {
		t.Run(format, func(t *testing.T) {
			extract, src := archive()
			if err := extract(src, t.TempDir(), MaxExtractSize); err == nil {
				t.Error("an entry replaced another one")
			}
		})
	}
}

func TestExtractZipBackslashSeparator(t *testing.T) {
	src := writeArchive(t, "a.zip", zipBytes(t, archiveEntry{name: `dir\file.txt`, content: "x"}))
	dstDir := t.TempDir()
	if err := ExtractZip(src, dstDir, MaxExtractSize); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "dir", "file.txt")); err != nil {
		t.Error(err)
	}
}

func TestExtractInvalidArchive(t *testing.T) {
	src := writeArchive(t, "a.tar.gz", []byte("not an archive"))
	if err := ExtractTarGz(src, t.TempDir(), MaxExtractSize); err == nil || !strings.Contains(err.Error(), "a.tar.gz") {
		t.Errorf("tar.gz: %v", err)
	}
	if err := ExtractZip(src, t.TempDir(), MaxExtractSize); err == nil || !strings.Contains(err.Error(), "a.tar.gz") {
		t.Errorf("zip: %v", err)
	}
	if err := ExtractTarGz(filepath.Join(t.TempDir(), "missing"), t.TempDir(), MaxExtractSize); err == nil {
		t.Error("a missing archive extracted")
	}

	truncated := tarGzBytes(t, archiveEntry{name: "a", content: strings.Repeat("x", 4096)})
	src = writeArchive(t, "b.tar.gz", truncated[:len(truncated)/2])
	if err := ExtractTarGz(src, t.TempDir(), MaxExtractSize); err == nil {
		t.Error("a truncated archive extracted")
	}
}
