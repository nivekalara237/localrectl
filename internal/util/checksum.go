package util

import (
	cryptomd5 "crypto/md5"
	cryptoSha256 "crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

func Sha265(b []byte) string {
	h := cryptoSha256.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func Sha256String(str string) string {
	return Sha265([]byte(str))
}

func MD5(b []byte) string {
	h := cryptomd5.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Sha256File returns the sha256 of the file at path, read as a stream: a large file is never
// loaded in memory.
func Sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := cryptoSha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
