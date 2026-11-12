package util

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var reservedFilenames = map[string]map[string]string{
	"index.json":                {},
	"release.json":              {},
	"artifacts.goreleaser.json": {},
}

// CreateSoftTempdir creates a new temporary directory and returns its path. The caller removes
// it with os.RemoveAll.
func CreateSoftTempdir() (string, error) {
	return os.MkdirTemp("", "localrectl-*")
}

// within reports whether p is root or below it (after Clean).
func within(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

type Resolved struct {
	Path string
	Info fs.FileInfo
}

// ResolveEntry resolves one manifest entry against assetDir (relative or absolute),
// tolerating both "base prefix" and "subdirectory" interpretations.
func ResolveEntry(assetDir, raw string) (Resolved, error) {
	absRoot, err := filepath.Abs(assetDir)
	if err != nil {
		return Resolved{}, err
	}
	absRoot = filepath.Clean(absRoot)

	// Build candidate paths.
	var cands []string
	if filepath.IsAbs(raw) {
		cands = []string{filepath.Clean(raw)}
	} else {
		// collapses "./", "a/./b", etc.
		raw = filepath.Clean(raw)
		// subdir interpretation
		cands = append(cands, filepath.Join(absRoot, raw))

		base := filepath.Base(absRoot)
		prefix := base + string(filepath.Separator)
		if strings.HasPrefix(raw, prefix) {
			// strip-base interpretation
			cands = append(cands, filepath.Join(absRoot, raw[len(prefix):]))
		}
	}

	// Keep only valid matches.
	var ok []Resolved
	for _, c := range cands {
		_, err = filepath.EvalSymlinks(c)
		if err != nil || !within(absRoot, c) {
			continue
		}
		if _, reserved := reservedFilenames[filepath.Base(c)]; reserved {
			continue
		}
		info, err := os.Stat(c) // follows symlinks — matches [ -f ]
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		ok = append(ok, Resolved{c, info})
	}

	// Deduplicate by path (both interpretations can land on the same file).
	seen := map[string]struct{}{}
	var uniq []Resolved
	for _, r := range ok {
		if _, dup := seen[r.Path]; !dup {
			seen[r.Path] = struct{}{}
			uniq = append(uniq, r)
		}
	}

	switch len(uniq) {
	case 0:
		return Resolved{}, fmt.Errorf("%q: no valid file under %s (tried %v)", raw, absRoot, cands)
	case 1:
		return uniq[0], nil
	default:
		paths := make([]string, len(uniq))
		for i, r := range uniq {
			paths[i] = r.Path
		}
		return Resolved{}, fmt.Errorf("%q is ambiguous, matches: %v", raw, paths)
	}
}
