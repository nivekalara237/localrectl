package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveEntry(t *testing.T) {
	// t.TempDir is under /var on macOS, a link to /private/var: the root itself is a link.
	dir := t.TempDir()
	root := filepath.Join(dir, "dist")
	outside := filepath.Join(dir, "secret")
	writeFile(t, filepath.Join(root, "a.tar.gz"), "a")
	writeFile(t, filepath.Join(root, "sub", "b.zip"), "b")
	writeFile(t, outside, "secret")
	writeFile(t, filepath.Join(dir, "other", "c.txt"), "c")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a.tar.gz"), filepath.Join(root, "inner-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "other"), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		raw, want string
	}{
		{"a.tar.gz", filepath.Join(root, "a.tar.gz")},
		{"dist/a.tar.gz", filepath.Join(root, "a.tar.gz")},
		{"sub/b.zip", filepath.Join(root, "sub", "b.zip")},
		{filepath.Join(root, "a.tar.gz"), filepath.Join(root, "a.tar.gz")},
		{"link", ""},             // a link to a file out of the root
		{"inner-link", ""},       // a link, even inside the root
		{"linked-dir/c.txt", ""}, // a linked directory leading out of the root
		{"../secret", ""},
		{outside, ""},
		{"sub", ""}, // a directory
		{"missing", ""},
	} {
		got, err := ResolveEntry(root, tc.raw)
		if tc.want == "" {
			if err == nil {
				t.Errorf("ResolveEntry(%q) = %s, want an error", tc.raw, got.Path)
			}
			continue
		}
		if err != nil || got.Path != tc.want {
			t.Errorf("ResolveEntry(%q) = %q, %v, want %q", tc.raw, got.Path, err, tc.want)
		}
	}

	if _, err := ResolveEntry(root, "link"); err == nil || !strings.Contains(err.Error(), "symbolic links refused") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveEntryMissingRoot(t *testing.T) {
	if _, err := ResolveEntry(filepath.Join(t.TempDir(), "none"), "a"); err == nil {
		t.Error("no error for a missing root")
	}
}
