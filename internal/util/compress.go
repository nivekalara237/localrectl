package util

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func GzipFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func(in *os.File) {
		_ = in.Close()
	}(in)

	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	gz := gzip.NewWriter(tmp)
	gz.Name = info.Name()
	gz.ModTime = info.ModTime()

	_, copyErr := io.Copy(gz, in)
	closeErr := gz.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return err
	}
	success = true
	return nil
}

func ZipFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func(in *os.File) {
		_ = in.Close()
	}(in)

	info, err := in.Stat() // fstat the fd we actually read
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	zw := zip.NewWriter(tmp)
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Method = zip.Deflate
	header.Name = filepath.Base(src)

	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	if _, err = io.Copy(w, in); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return err
	}
	success = true
	return nil
}

// hidden tells whether the walk of srcDir skips path: a name starting with "." (.env, .git,
// .DS_Store) is never archived, as it may hold a secret.
func hidden(srcDir, path string, d fs.DirEntry) bool {
	return path != srcDir && strings.HasPrefix(d.Name(), ".")
}

// skipHidden returns the answer of a WalkDir function for a hidden path: skip it, and all of
// it for a directory.
func skipHidden(d fs.DirEntry) error {
	if d.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// sourceDateEpoch returns the time of SOURCE_DATE_EPOCH (https://reproducible-builds.org), the
// date of every archived file when it is set.
func sourceDateEpoch() (time.Time, bool) {
	sec, err := strconv.ParseInt(os.Getenv("SOURCE_DATE_EPOCH"), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
}

// TarGzDir streams srcDir into a gzip-compressed tar at dst. Hidden files are skipped (see
// hidden), and the owner is not kept, so the archive of the same files is the same on every
// machine: with SOURCE_DATE_EPOCH, the dates too.
func TarGzDir(srcDir, dst string) (err error) {
	tmpDir := filepath.Dir(dst)
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	gz, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)

	// Correct close order: tar (writes end-of-archive) → gzip (writes
	// trailer) → file. Propagate the first error.
	closeAll := func() error {
		var first error
		for _, c := range []func() error{tw.Close, gz.Close, tmp.Close} {
			if e := c(); e != nil && first == nil {
				first = e
			}
		}
		return first
	}

	walkErr := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Don't archive the output / temp file if they live inside srcDir.
		if path == tmpName || path == dst {
			return nil
		}
		if hidden(srcDir, path, d) {
			return skipHidden(d)
		}

		info, err := d.Info() // lstat semantics — symlinks are NOT followed
		if err != nil {
			return err
		}

		// Only regular files (plus directory entries for structure).
		if !d.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		// tar paths always use forward slashes
		rel = filepath.ToSlash(rel)

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if epoch, ok := sourceDateEpoch(); ok {
			hdr.ModTime, hdr.AccessTime, hdr.ChangeTime = epoch, time.Time{}, time.Time{}
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func(f *os.File) {
			_ = f.Close()
		}(f)
		_, err = io.Copy(tw, f)
		return err
	})

	if walkErr != nil {
		_ = closeAll() // best effort; defer removes the temp file
		return walkErr
	}

	if err := closeAll(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	success = true
	return nil
}

// ZipDir writes srcDir into a zip archive at dst, with the same layout and rules as TarGzDir.
func ZipDir(srcDir, dst string) (err error) {
	tmpDir := filepath.Dir(dst)
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	zw := zip.NewWriter(tmp)

	walkErr := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == tmpName || path == dst {
			return nil
		}
		if hidden(srcDir, path, d) {
			return skipHidden(d)
		}

		info, err := d.Info() // lstat semantics — symlinks are NOT followed
		if err != nil {
			return err
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		// zip paths always use forward slashes
		hdr.Name = filepath.ToSlash(rel)
		if epoch, ok := sourceDateEpoch(); ok {
			hdr.Modified = epoch
		}
		if d.IsDir() {
			hdr.Name += "/"
			_, err = zw.CreateHeader(hdr)
			return err
		}
		hdr.Method = zip.Deflate

		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func(f *os.File) {
			_ = f.Close()
		}(f)
		_, err = io.Copy(w, f)
		return err
	})

	if walkErr != nil {
		_ = zw.Close()
		return walkErr
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	success = true
	return nil
}
