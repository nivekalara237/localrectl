package util

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MaxExtractSize bounds the bytes one extraction writes, so a small archive cannot fill the disk.
const MaxExtractSize int64 = 8 << 30

// ExtractTarGz extracts the gzip-compressed tar src into dstDir, which must be empty. Only
// directories and regular files are accepted: a link, a device, or an entry that would land
// outside dstDir fails, as does an extraction above limit bytes.
func ExtractTarGz(src, dstDir string, limit int64) error {
	archive, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close() }()

	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(src), err)
	}
	defer func() { _ = gz.Close() }()

	extractor := &extractor{dstDir: dstDir, limit: limit, remaining: limit}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(src), err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = extractor.dir(header.Name)
		case tar.TypeReg:
			err = extractor.file(header.Name, header.FileInfo().Mode(), reader)
		case tar.TypeXGlobalHeader:
			continue
		default:
			err = fmt.Errorf("%s is not a regular file nor a directory", header.Name)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(src), err)
		}
	}
}

// ExtractZip extracts the zip archive src into dstDir, with the same rules as ExtractTarGz.
func ExtractZip(src, dstDir string, limit int64) error {
	archive, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(src), err)
	}
	defer func() { _ = archive.Close() }()

	extractor := &extractor{dstDir: dstDir, limit: limit, remaining: limit}
	for _, entry := range archive.File {
		if err := extractor.zipEntry(entry); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(src), err)
		}
	}
	return nil
}

type extractor struct {
	dstDir    string
	limit     int64
	remaining int64
}

func (e *extractor) zipEntry(entry *zip.File) error {
	mode := entry.Mode()
	switch {
	case mode.IsDir():
		return e.dir(entry.Name)
	case !mode.IsRegular():
		return fmt.Errorf("%s is not a regular file nor a directory", entry.Name)
	}
	content, err := entry.Open()
	if err != nil {
		return fmt.Errorf("%s: %w", entry.Name, err)
	}
	defer func() { _ = content.Close() }()
	return e.file(entry.Name, mode, content)
}

// target returns the path of the entry name under dstDir. A name that is absolute, holds a ..
// segment, or leads out of dstDir is refused; a \ counts as a separator, as on Windows.
func (e *extractor) target(name string) (string, error) {
	slashed := strings.ReplaceAll(name, `\`, "/")
	if slashed == "" || strings.HasPrefix(slashed, "/") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("refused entry %q: absolute or empty path", name)
	}
	for _, segment := range strings.Split(slashed, "/") {
		if segment == ".." {
			return "", fmt.Errorf("refused entry %q: it leads out of the destination", name)
		}
	}
	path := filepath.Join(e.dstDir, filepath.FromSlash(slashed))
	if !within(e.dstDir, path) {
		return "", fmt.Errorf("refused entry %q: it leads out of the destination", name)
	}
	return path, nil
}

func (e *extractor) dir(name string) error {
	path, err := e.target(name)
	if err != nil {
		return err
	}
	return os.MkdirAll(path, 0o755)
}

// file writes one regular file. It never replaces a file, so an archive that holds the same
// name twice fails. The file is 0755 when the archive gives it an execute bit, else 0644.
func (e *extractor) file(name string, mode fs.FileMode, content io.Reader) error {
	path, err := e.target(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if mode.Perm()&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, io.LimitReader(content, e.remaining+1))
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("%s: %w", name, copyErr)
	}
	if written > e.remaining {
		return fmt.Errorf("the extracted files exceed %d bytes", e.limit)
	}
	e.remaining -= written
	return nil
}
