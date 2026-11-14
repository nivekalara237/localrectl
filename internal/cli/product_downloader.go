package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/httpclient"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/nivekalara237/localrectl/internal/util"
	"github.com/spf13/cobra"
)

type downloaderCmd struct {
	cfg         *config.Config
	log         *slog.Logger
	printer     *logging.Printer
	flags       *pdFlags
	productName string
	version     string
	tempPaths   []string
}

const (
	statusDownloaded = "downloaded"
	statusUpToDate   = "up-to-date"
	statusDryRun     = "dry-run"

	kindChecksums = "checksums"
)

var kinds = []string{"archive", "binary", "package", "file", "source"}

var operatingSystems = []string{"linux", "darwin", "windows"}

type pdFlags struct {
	os              string
	arch            string
	kind            string
	files           []string
	all             bool
	destDir         string
	extract         bool
	noVerify        bool
	verifySignature bool
	force           bool
	dryRun          bool

	key                       string
	certificateIdentity       string
	certificateIdentityRegexp string
	certificateOIDCIssuer     string
}

type downloadedFile struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Size      int64    `json:"size"`
	Sha256    string   `json:"sha256"`
	Path      string   `json:"path"`
	Status    string   `json:"status"`
	Verified  bool     `json:"verified"`
	Extracted []string `json:"extracted,omitempty"`
}

type downloadSummary struct {
	Product           string           `json:"product"`
	Version           string           `json:"version"`
	Dir               string           `json:"dir"`
	TotalSize         int64            `json:"total_size"`
	DryRun            bool             `json:"dry_run"`
	SignatureVerified bool             `json:"signature_verified"`
	Files             []downloadedFile `json:"files"`
}

// validateFlags checks the values of the flags and the flags used together. macos is taken as
// darwin, the name the server uses.
func (f *pdFlags) validateFlags() error {
	if f.os == "macos" {
		f.os = "darwin"
	}
	if f.os != "" && !slices.Contains(operatingSystems, f.os) {
		return config.Usage(fmt.Errorf("--os must be one of %s (or macos), not %s", strings.Join(operatingSystems, ", "), f.os))
	}
	if f.kind != "" && !slices.Contains(kinds, f.kind) {
		return config.Usage(fmt.Errorf("--kind must be one of %s, not %s", strings.Join(kinds, ", "), f.kind))
	}
	if f.all && (len(f.files) > 0 || f.hasFilter()) {
		return config.Usage(errors.New("--all cannot be used with --file, --os, --arch or --kind"))
	}
	if len(f.files) > 0 && f.hasFilter() {
		return config.Usage(errors.New("--file cannot be used with --os, --arch or --kind"))
	}
	return f.validateSignatureFlags()
}

func (f *pdFlags) hasFilter() bool {
	return f.os != "" || f.arch != "" || f.kind != ""
}

func newDownloaderCmd(cfg *config.Config, printer *logging.Printer, flags *pdFlags, productName, version string) *downloaderCmd {
	return &downloaderCmd{
		cfg:         cfg,
		log:         printer.Logger().With("cmd", "download"),
		printer:     printer,
		flags:       flags,
		productName: productName,
		version:     version,
	}
}

func newProductDownloaderCommand() *cobra.Command {
	flags := &pdFlags{}
	cmd := cobra.Command{
		Use:   "download <product-name> [version]",
		Short: "Download the files of a version of a product, and check their sha256",
		Long: `Download the files of a version of a product, and check each one against the sha256
of its release.json (GET <server-url>/<product-name>/releases/<version>, then
GET <server-url>/<product-name>/artifacts/<version>/files/<name> for each file).

[version] is a version listed by 'localrectl versions <product-name>', or latest, the default.

The files are chosen from the release.json, never by guessing their name:
  - --file NAME: these files, by name (repeatable);
  - --all: every file of the version, the checksums file included;
  - --os, --arch, --kind: every file that matches. A file with no os or arch fits any;
  - none of them: the archive of this machine (its os and arch), a zip on Windows and a
    tar.gz elsewhere when the version has both.

Each file is written to a temporary file in --dir, checked, then renamed: a corrupted file
never reaches its destination. A file already there with the same sha256 is not downloaded
again; a different one fails, unless --force replaces it.

--extract extracts each downloaded archive (tar.gz or zip) into --dir once every file is
checked. Only directories and regular files are extracted: a link, or an entry that would
land outside --dir, fails. The archive is kept. An entry already in --dir fails, unless
--force replaces it.

--verify-signature checks the Sigstore signature of the checksums file with cosign, which must
be installed, before anything is downloaded; then each file must be listed in that signed file
with the sha256 of the release.json. The expected signer is given by --key, its public key, or
by --certificate-identity (or --certificate-identity-regexp) with --certificate-oidc-issuer.`,
		Example: `  # The archive of this machine, for the latest version, in the current directory
  localrectl download billing-agent

  # The linux arm64 files of a version, in /opt/billing-agent
  localrectl download billing-agent v1.2.0 --os linux --arch arm64 -d /opt/billing-agent

  # Precise files
  localrectl download billing-agent v1.2.0 --file billing-agent_1.2.0_checksums.txt --file billing-agent_1.2.0_linux_amd64.tar.gz

  # The archive of this machine, extracted in /opt/billing-agent
  localrectl download billing-agent --extract -d /opt/billing-agent

  # Signed by the release workflow of a GitHub repository, with cosign keyless
  localrectl download billing-agent --verify-signature \
    --certificate-identity-regexp '^https://github.com/acme/billing-agent/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

  # Every file of the version
  localrectl download billing-agent v1.2.0 --all -d ./billing-agent

  # What would be downloaded, without downloading anything
  localrectl download billing-agent --all --dry-run`,
		Aliases: []string{"downloader", "product-download"},
		Args:    downloadArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			printer := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("unable to load the configuration")
			}
			if err := flags.validateFlags(); err != nil {
				return err
			}

			productName := args[0]
			version := "latest"
			if len(args) == 2 {
				version = args[1]
			}

			// Ctrl+C or SIGTERM cancels the requests, so the command returns and its deferred
			// cleanup runs. Once caught, the signals get their default action back.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()
			cmd.SetContext(ctx)

			downloader := newDownloaderCmd(cfg, printer, flags, productName, version)
			defer downloader.removeTemps()

			result, err := downloader.runDownloader(cmd)
			if err != nil {
				return err
			}
			return printer.Result(result, func(w io.Writer) error {
				status := ""
				if result.DryRun {
					status = " (dry run)"
				}
				_, err := fmt.Fprintf(w, "%s %s: %d files (%s) in %s%s\n", result.Product, result.Version,
					len(result.Files), humanBytes(result.TotalSize), result.Dir, status)
				return err
			})
		},
	}

	flagSet := cmd.Flags()
	flagSet.StringVar(&flags.os, "os", "", "Operating system of the files: linux, darwin (or macos) or windows (default: this machine's)")
	flagSet.StringVar(&flags.arch, "arch", "", "Architecture of the files, such as amd64 or arm64 (default: this machine's)")
	flagSet.StringVar(&flags.kind, "kind", "", "Kind of the files: "+strings.Join(kinds, ", ")+" (default: archive)")
	flagSet.StringArrayVar(&flags.files, "file", nil, "File to download, by name (repeatable)")
	flagSet.BoolVar(&flags.all, "all", false, "Download every file of the version")
	flagSet.StringVarP(&flags.destDir, "dir", "d", "", "Destination directory, created if missing (default: the current directory)")
	flagSet.BoolVar(&flags.force, "force", false, "Replace a file of the destination that differs from the downloaded one, or an extracted entry")
	flagSet.BoolVar(&flags.dryRun, "dry-run", false, "Show what would be downloaded, without downloading anything")
	flagSet.BoolVar(&flags.noVerify, "no-verify", false, "Skip the sha256 check (not recommended)")
	flagSet.BoolVar(&flags.extract, "extract", false, "Extract the downloaded archives into --dir once checked; the archives are kept")
	flagSet.BoolVar(&flags.verifySignature, "verify-signature", false, "Check the Sigstore signature of the checksums file with cosign, then each file against it")
	flagSet.StringVar(&flags.key, "key", "", "Public key of the signer, for --verify-signature")
	flagSet.StringVar(&flags.certificateIdentity, "certificate-identity", "", "Identity of the signer in its certificate, for --verify-signature")
	flagSet.StringVar(&flags.certificateIdentityRegexp, "certificate-identity-regexp", "", "Regular expression of the identity of the signer, for --verify-signature")
	flagSet.StringVar(&flags.certificateOIDCIssuer, "certificate-oidc-issuer", "", "Issuer of the identity of the signer, such as https://token.actions.githubusercontent.com, for --verify-signature")
	return &cmd
}

// downloadArgs accepts <product-name> [version], each one a single path segment.
func downloadArgs(cmd *cobra.Command, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return config.Usage(fmt.Errorf("the command %s waits 1 or 2 args: <product-name> [version]", cmd.CommandPath()))
	}
	if err := pathSegmentArg("product name", args[0]); err != nil {
		return config.Usage(err)
	}
	if len(args) == 2 {
		return pathSegmentArg("version", args[1])
	}
	return nil
}

// runDownloader reads the release.json of the version, chooses the files, then downloads and
// checks them one after the other.
func (d *downloaderCmd) runDownloader(cmd *cobra.Command) (*downloadSummary, error) {
	ctx := cmd.Context()
	release, err := d.fetchRelease(cmd)
	if err != nil {
		return nil, err
	}
	d.version = release.Version

	selected, err := d.selectArtifacts(release)
	if err != nil {
		return nil, err
	}

	destDir, err := d.destinationDir()
	if err != nil {
		return nil, err
	}
	if d.flags.noVerify {
		d.log.WarnContext(ctx, "--no-verify: the sha256 of the downloaded files is not checked")
	}
	if d.flags.verifySignature {
		if selected, err = d.verifySignature(ctx, release, selected); err != nil {
			return nil, err
		}
	}

	summary := &downloadSummary{Product: d.productName, Version: d.version, Dir: destDir, DryRun: d.flags.dryRun,
		SignatureVerified: d.flags.verifySignature && !d.flags.dryRun}
	for index, artifact := range selected {
		file, err := d.download(ctx, index+1, len(selected), artifact, destDir)
		if err != nil {
			return nil, err
		}
		summary.TotalSize += file.Size
		summary.Files = append(summary.Files, file)
	}
	if d.flags.extract {
		for index, artifact := range selected {
			if err := d.extract(&summary.Files[index], artifact, destDir); err != nil {
				return nil, err
			}
		}
	}
	return summary, nil
}

// extract extracts a downloaded archive into destDir, once every file is downloaded and checked.
// The archive goes to a temporary directory of destDir first, then each of its top entries is
// moved to destDir: an entry already there fails before anything moves, unless --force
// replaces it. A file that is not an archive is left as it is.
func (d *downloaderCmd) extract(file *downloadedFile, artifact releaseArtifact, destDir string) error {
	if artifact.Kind != "archive" {
		d.log.Info("not an archive: not extracted", "file", artifact.Name)
		return nil
	}
	extractArchive := util.ExtractTarGz
	switch artifact.archiveFormat() {
	case "tar.gz":
	case "zip":
		extractArchive = util.ExtractZip
	default:
		return fmt.Errorf("%s: unknown archive format, it cannot be extracted", artifact.Name)
	}
	if d.flags.dryRun {
		_, _ = fmt.Fprintf(d.printer.Err, "%s  extracted into %s (not done: dry run)\n", artifact.Name, destDir)
		return nil
	}

	extractDir, err := os.MkdirTemp(destDir, ".extract-*")
	if err != nil {
		return err
	}
	d.tempPaths = append(d.tempPaths, extractDir)
	if err := extractArchive(file.Path, extractDir, util.MaxExtractSize); err != nil {
		return fmt.Errorf("extract %w", err)
	}

	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return err
	}
	var existing []string
	for _, entry := range entries {
		if _, err := os.Lstat(filepath.Join(destDir, entry.Name())); err == nil {
			existing = append(existing, entry.Name())
		}
	}
	if len(existing) > 0 && !d.flags.force {
		return fmt.Errorf("extracting %s would replace %s in %s: pass --force to replace them",
			artifact.Name, strings.Join(existing, ", "), destDir)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		target := filepath.Join(destDir, entry.Name())
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(extractDir, entry.Name()), target); err != nil {
			return err
		}
		file.Extracted = append(file.Extracted, target)
		names = append(names, entry.Name())
	}
	_, _ = fmt.Fprintf(d.printer.Err, "%s  extracted: %s\n", artifact.Name, orDefault(strings.Join(names, ", "), "nothing"))
	return nil
}

// fetchRelease reads the release.json of the version through the release command. The version
// it holds replaces latest.
func (d *downloaderCmd) fetchRelease(cmd *cobra.Command) (*releaseDoc, error) {
	raw, err := newProductReleaseCmd(d.cfg, d.log).runProductRelease(cmd, d.productName, d.version)
	if err != nil {
		return nil, err
	}
	var release releaseDoc
	if err := json.Unmarshal(raw.raw, &release); err != nil {
		return nil, fmt.Errorf("read the release.json of %s %s: %w", d.productName, d.version, err)
	}
	if release.Version == "" {
		if d.version == "latest" {
			return nil, fmt.Errorf("the release.json of %s latest has no version", d.productName)
		}
		release.Version = d.version
	}
	if err := pathSegmentArg("version", release.Version); err != nil {
		return nil, fmt.Errorf("the release.json of %s %s: %w", d.productName, d.version, err)
	}
	return &release, nil
}

// selectArtifacts chooses the files to download from the release: --file, else --all, else
// the filters. Without any filter, it keeps the one archive of this machine.
func (d *downloaderCmd) selectArtifacts(release *releaseDoc) ([]releaseArtifact, error) {
	available := release.files()
	if len(d.flags.files) > 0 {
		selected := make([]releaseArtifact, 0, len(d.flags.files))
		for _, name := range d.flags.files {
			index := slices.IndexFunc(available, func(a releaseArtifact) bool { return a.Name == name })
			if index < 0 {
				return nil, fmt.Errorf("%s %s has no file %s; its files: %s", d.productName, d.version, name, fileNames(available))
			}
			selected = append(selected, available[index])
		}
		return selected, nil
	}
	if d.flags.all {
		if len(available) == 0 {
			return nil, fmt.Errorf("%s %s has no file", d.productName, d.version)
		}
		return available, nil
	}

	osName := orDefault(d.flags.os, runtime.GOOS)
	arch := orDefault(d.flags.arch, runtime.GOARCH)
	kind := orDefault(d.flags.kind, "archive")
	var matches []releaseArtifact
	for _, artifact := range release.Artifacts {
		if artifact.Kind == kind && fits(artifact.OS, osName) && fits(artifact.Arch, arch) {
			matches = append(matches, artifact)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s %s has no %s for %s/%s; its files: %s (pick one with --file)",
			d.productName, d.version, kind, osName, arch, fileNames(available))
	}
	if d.flags.hasFilter() || len(matches) == 1 {
		return matches, nil
	}

	preferred := "tar.gz"
	if osName == "windows" {
		preferred = "zip"
	}
	matches = slices.DeleteFunc(matches, func(a releaseArtifact) bool { return a.archiveFormat() != preferred })
	if len(matches) != 1 {
		return nil, fmt.Errorf("%s %s has several archives for %s/%s; pick one with --file among: %s",
			d.productName, d.version, osName, arch, fileNames(available))
	}
	return matches, nil
}

// destinationDir returns the absolute path of --dir, the current directory by default, and
// creates it unless the run is a dry run.
func (d *downloaderCmd) destinationDir() (string, error) {
	dir, err := config.ExpandPath(orDefault(d.flags.destDir, "."))
	if err != nil {
		return "", fmt.Errorf("the destination directory %s: %w", d.flags.destDir, err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("the destination %s is not a directory", dir)
	case err == nil, errors.Is(err, os.ErrNotExist) && d.flags.dryRun:
		return dir, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("the destination directory %s cannot be read: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create the destination directory %s: %w", dir, err)
	}
	return dir, nil
}

// download writes one file of the release to destDir and checks its sha256. A file already
// there is kept when its sha256 is the expected one, replaced with --force, else an error.
func (d *downloaderCmd) download(ctx context.Context, number, total int, artifact releaseArtifact, destDir string) (downloadedFile, error) {
	if err := safeFileName(artifact.Name); err != nil {
		return downloadedFile{}, err
	}
	label := fmt.Sprintf("[%d/%d] %s", number, total, artifact.Name)
	target := filepath.Join(destDir, artifact.Name)
	file := downloadedFile{Name: artifact.Name, Kind: artifact.Kind, Size: artifact.Size, Sha256: artifact.Sha256, Path: target}

	upToDate, err := d.checkExisting(artifact, target)
	if err != nil {
		return downloadedFile{}, err
	}
	if upToDate {
		file.Status, file.Verified = statusUpToDate, true
		_, _ = fmt.Fprintf(d.printer.Err, "%s  %s  already up to date\n", label, humanBytes(artifact.Size))
		return file, nil
	}
	if d.flags.dryRun {
		file.Status = statusDryRun
		_, _ = fmt.Fprintf(d.printer.Err, "%s  %s  sha256 %s (not downloaded: dry run)\n", label,
			humanBytes(artifact.Size), orDefault(artifact.Sha256, "unknown"))
		return file, nil
	}

	verify := !d.flags.noVerify && artifact.Kind != kindChecksums
	if verify && artifact.Sha256 == "" {
		return downloadedFile{}, fmt.Errorf("the release.json gives no sha256 for %s: pass --no-verify to download it anyway", artifact.Name)
	}

	progress := newProgressBar(d.printer.Err, label, d.printer.UseColor())
	partPath, size, checksum, err := d.fetchFile(ctx, artifact.Name, destDir, progress)
	if err == nil && verify {
		err = checkDownload(artifact, size, checksum)
	}
	if err == nil {
		err = install(partPath, target, artifact.Kind)
	}
	if err != nil {
		progress.Fail()
		return downloadedFile{}, err
	}
	progress.Done()

	file.Size, file.Sha256, file.Status, file.Verified = size, checksum, statusDownloaded, verify
	return file, nil
}

// checkExisting tells whether target already holds the expected file. A different file is an
// error, unless --force lets the download replace it.
func (d *downloaderCmd) checkExisting(artifact releaseArtifact, target string) (bool, error) {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("the file %s cannot be read: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		if d.flags.force && info.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
		return false, fmt.Errorf("%s already exists and is not a regular file", target)
	}
	if artifact.Sha256 != "" {
		checksum, err := util.Sha256File(target)
		if err != nil {
			return false, err
		}
		if strings.EqualFold(checksum, artifact.Sha256) {
			return true, nil
		}
	}
	if d.flags.force {
		return false, nil
	}
	return false, fmt.Errorf("%s already exists and differs from the file of the release: pass --force to replace it", target)
}

// fetchFile downloads the file name of the version into a temporary file of destDir. It returns
// the path of that file, with the size and the sha256 of the bytes received.
func (d *downloaderCmd) fetchFile(ctx context.Context, name, destDir string, progress *progressBar) (string, int64, string, error) {
	endpoint := fmt.Sprintf(productUploadEndpoint, d.productName, d.version, name)
	start := time.Now()
	resp, err := d.cfg.HttpClient().Get(ctx, endpoint, &httpclient.RequestOptions{DownloadProgress: progress.Update})
	if err != nil {
		var clientErr *httpclient.ClientError
		if errors.As(err, &clientErr) && clientErr.Message != "" {
			err = errors.New(clientErr.Message)
		}
		return "", 0, "", fmt.Errorf("download %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	d.log.DebugContext(ctx, "request", "method", http.MethodGet, "endpoint", endpoint,
		"status", resp.StatusCode, "duration", time.Since(start))

	if resp.StatusCode == http.StatusNotFound {
		return "", 0, "", fmt.Errorf("download %s: not found on the server", name)
	}
	if !httpclient.IsSuccess(resp) {
		return "", 0, "", fmt.Errorf("download %s: %w", name, serverRefusal(resp))
	}

	part, err := os.CreateTemp(destDir, "."+name+".part-*")
	if err != nil {
		return "", 0, "", err
	}
	d.tempPaths = append(d.tempPaths, part.Name())
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(part, hash), resp.Body)
	if closeErr := part.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return "", 0, "", fmt.Errorf("download %s: %w", name, copyErr)
	}
	return part.Name(), size, hex.EncodeToString(hash.Sum(nil)), nil
}

// install gives the downloaded file its permissions, 0755 for a binary outside Windows, else
// 0644, then moves it to target.
func install(partPath, target, kind string) error {
	mode := os.FileMode(0o644)
	if kind == "binary" && runtime.GOOS != "windows" {
		mode = 0o755
	}
	if err := os.Chmod(partPath, mode); err != nil {
		return err
	}
	return os.Rename(partPath, target)
}

// removeTemps removes the temporary files and directories of the downloads and extractions,
// left behind by a failure, a cancellation or a completed extraction.
func (d *downloaderCmd) removeTemps() {
	for _, tempPath := range d.tempPaths {
		if err := os.RemoveAll(tempPath); err != nil {
			d.log.Warn("unable to remove a temporary path", "path", tempPath, "error", err)
		}
	}
	d.tempPaths = nil
}

// checkDownload compares the size and the sha256 of the bytes received with the release.
func checkDownload(artifact releaseArtifact, size int64, checksum string) error {
	if artifact.Size > 0 && size != artifact.Size {
		return fmt.Errorf("%s: %d bytes received, the release gives %d", artifact.Name, size, artifact.Size)
	}
	if !strings.EqualFold(checksum, artifact.Sha256) {
		return fmt.Errorf("%s: sha256 mismatch: received %s, the release gives %s", artifact.Name, checksum, artifact.Sha256)
	}
	return nil
}

// safeFileName refuses a file name of the server that would write outside the destination
// directory, or hide the file.
func safeFileName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) ||
		strings.HasPrefix(name, ".") || filepath.IsAbs(name) {
		return fmt.Errorf("the release.json holds an invalid file name %q", name)
	}
	return nil
}

// fits tells whether a platform field of the release, os or arch, matches want. A file with no
// value fits any platform.
func fits(value *string, want string) bool {
	return value == nil || *value == "" || *value == want
}

// orDefault returns value, or fallback when value is empty.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func fileNames(artifacts []releaseArtifact) string {
	if len(artifacts) == 0 {
		return "none"
	}
	names := make([]string, len(artifacts))
	for index, artifact := range artifacts {
		names[index] = artifact.Name
	}
	return strings.Join(names, ", ")
}
