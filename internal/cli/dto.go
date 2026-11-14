package cli

import (
	"slices"
	"strings"
)

type artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Type   string `json:"type"`
	Goos   string `json:"goos"`
	Goarch string `json:"goarch"`
	Extra  struct {
		Format   any `json:"Format"`
		Checksum any `json:"Checksum"`
	} `json:"extra"`
}

type releaseDoc struct {
	Product   string            `json:"product"`
	Version   string            `json:"version"`
	Artifacts []releaseArtifact `json:"artifacts"`
	Checksums *releaseChecksums `json:"checksums"`
}

type releaseArtifact struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Format *string `json:"format"`
	OS     *string `json:"os"`
	Arch   *string `json:"arch"`
	Size   int64   `json:"size"`
	Sha256 string  `json:"sha256"`
}

type releaseChecksums struct {
	Name        string  `json:"name"`
	Signature   *string `json:"signature"`
	Certificate *string `json:"certificate"`
}

// files returns the artifacts of the release, then its checksums file, of kind checksums.
func (r *releaseDoc) files() []releaseArtifact {
	files := slices.Clone(r.Artifacts)
	if r.Checksums != nil && r.Checksums.Name != "" {
		files = append(files, releaseArtifact{Name: r.Checksums.Name, Kind: kindChecksums})
	}
	return files
}

// archiveFormat returns the format of an archive, tar.gz or zip, from the release or else from
// its name.
func (a releaseArtifact) archiveFormat() string {
	if a.Format != nil && *a.Format != "" {
		return *a.Format
	}
	switch {
	case strings.HasSuffix(a.Name, ".zip"):
		return "zip"
	case strings.HasSuffix(a.Name, ".tar.gz"), strings.HasSuffix(a.Name, ".tgz"):
		return "tar.gz"
	}
	return ""
}
