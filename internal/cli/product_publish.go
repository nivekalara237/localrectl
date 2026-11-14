package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/httpclient"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/nivekalara237/localrectl/internal/util"
	"github.com/spf13/cobra"
)

type publishCmd struct {
	log     *slog.Logger
	printer *logging.Printer
	cfg     *config.Config
	pFlags  *publishFlags
	pName   string

	totalSize int64    // bytes uploaded so far
	tmpDir    string   // the directory of the archives of --compress-subdirs, created at the first one
	tmpPaths  []string // temporary files and directories, removed by tmpCleanup
}

type summary struct {
	Total     int    `json:"total"`
	TotalSize int64  `json:"total_size"` // bytes uploaded
	Status    string `json:"status"`
	Version   string `json:"version"`
	Product   string `json:"product"`
}

type publishFlags struct {
	// latest also makes this version the latest one of the product.
	latest bool
	// dryRun shows what would be sent, and sends nothing: no token needed.
	dryRun bool
	// root is the directory of the files to send, and the one the paths of artifacts.json are
	// relative to.
	root string
	// version is the version to publish. By default, the version, else the tag, of metadata.json.
	version string
	// isNotSemverStandard accepts a version that is not X.Y.Z or X.Y.Z-pre.
	isNotSemverStandard bool
	// productFile is the product card, sent as registry.yaml.
	productFile string

	// changelog is the CHANGELOG.md to send with the version.
	changelog string
	// excludes are the files of the root directory not to send (*.log, .DS_Store).
	excludes []string
	// parallel is the number of files sent at the same time.
	parallel int
	// noValidate skips the checks made before anything is sent.
	noValidate bool
	// ifExists tells what to do when the version is already published: fail (the default), or
	// succeed without doing anything, to run a CI job again without an error.
	ifExists ifExists

	// For GoReleaser
	goReleaserArtifactsFile string
	goReleaserMetadataFile  string
	goReleaserDir           string // when set, goReleaserArtifactsFile and goReleaserMetadataFile are found in it

	// Directory walking
	skipSubdirectories     bool
	compressSubdirectories bool
	compressSubdirType     string
}
type ifExists string

var (
	fail ifExists = "fail"
	skip ifExists = "skip"
)

const (
	productPublishEndpoint = "%s/artifacts/%s/publish"
	productUploadEndpoint  = "%s/artifacts/%s/files/%s"
)

// The names the server reads the product card and the changelog by.
const (
	productCardName = "registry.yaml"
	changelogName   = "CHANGELOG.md"
)

var reservedFilenames = []string{
	"index.json",
	"release.json",
	"artifacts.goreleaser.json",
}
var artifactSupportedFileTypes = []string{"Archive", "File", "Source", "Linux Package", "Checksum",
	"Signature", "Certificate", "SBOM", "Metadata"}

// addTmpPath records a temporary file or directory for tmpCleanup.
func (p *publishCmd) addTmpPath(path string) {
	p.tmpPaths = append(p.tmpPaths, path)
}

// tmpCleanup removes every temporary file and directory recorded by addTmpPath. They stay on
// disk until the upload is done.
func (p *publishCmd) tmpCleanup() {
	for _, path := range p.tmpPaths {
		if err := os.RemoveAll(path); err != nil {
			p.log.Warn("unable to remove a temporary path", "path", path, "error", err)
		}
	}
	p.tmpPaths = nil
}

func newPublishCmd(pName string, printer *logging.Printer, cfg *config.Config, pf publishFlags) *publishCmd {
	return &publishCmd{
		log:     printer.Logger(),
		printer: printer,
		cfg:     cfg,
		pFlags:  &pf,
		pName:   pName,
	}
}

func newProductPublishCommand() *cobra.Command {
	flags := &publishFlags{}
	cmd := cobra.Command{
		Use:   "publish <product-name>",
		Short: "Upload the files of a new version of a product, then publish it",
		Long: `Upload the files of a new version of a product to the server, then publish it
(PUT <server-url>/<product-name>/artifacts/<version>/files/<name> for each file, then
POST <server-url>/<product-name>/artifacts/<version>/publish).

The files come from one of two places:
  - a goreleaser build: the files listed in its artifacts.json, with artifacts.json and
    metadata.json. It is used with --goreleaser-dir or --goreleaser-artifacts-file, or when
    the root directory holds an artifacts.json;
  - a plain directory, --root (the current directory by default): every regular file in it.
    A sub directory fails, unless --skip-subdirs skips it or --compress-subdirs sends it as
    one archive.

The product card, --product-file, is checked against the rules of the server, then sent as
registry.yaml. Every file is sent with its sha256, and the server checks it.

Publishing needs the upload token: set LOCAL_REGISTRY_UPLOAD_TOKEN, or server.upload-token in
the config file. --dry-run shows what would be sent, sends nothing and needs no token.

A published version never changes: publishing it again fails with "already published".`,
		Example: `  # The files of a directory
  localrectl publish billing-agent --root ./out --version 1.2.0 --product-file registry.yaml

  # A goreleaser build, made the latest version of the product
  localrectl publish billing-agent --goreleaser-dir dist --version 1.2.0 --product-file registry.yaml --latest

  # Each sub directory of ./out sent as a zip archive
  localrectl publish billing-agent --root ./out --version 1.2.0 --product-file registry.yaml \
    --compress-subdirs --compress-type zip

  # What would be sent, without sending anything
  localrectl publish billing-agent --root ./out --version 1.2.0 --product-file registry.yaml --dry-run`,
		Aliases: []string{"product-publish"},
		Args:    productNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("unable to load the configuration")
			}
			pname := args[0]

			// Ctrl+C or SIGTERM cancels the requests, so the command returns and its deferred
			// cleanup runs. Once caught, the signals get their default action back: a second
			// Ctrl+C stops the command at once.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				stop()
			}()
			cmd.SetContext(ctx)

			validationCmd := newValidateCmd(cfg, p.Logger())

			err := flags.validateFlags(pname, p.Logger(), ctx, validationCmd)
			if err != nil {
				return err
			}
			if err := flags.postValidateFlags(p.Logger(), ctx, validationCmd); err != nil {
				return err
			}

			pcmd := newPublishCmd(pname, p, cfg, *flags)
			defer pcmd.tmpCleanup()
			proceed, err := pcmd.processPublish(cmd)
			if err != nil {
				return err
			}
			return p.Result(proceed, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s %s: %s, %d files (%s)\n", proceed.Product, proceed.Version,
					proceed.Status, proceed.Total, humanBytes(proceed.TotalSize))
				return err
			})
		},
	}

	setFlagSet(&cmd, flags)

	return &cmd
}

func setFlagSet(cmd *cobra.Command, pflags *publishFlags) {
	flagSet := cmd.Flags()
	flagSet.StringVar(&pflags.version, "version", "", "Version to publish: X.Y.Z or X.Y.Z-pre, such as 1.2.0-rc.1 (default: the tag, else the version, of metadata.json)")
	flagSet.StringVar(&pflags.productFile, "product-file", "", "Product card to check, then send as registry.yaml")
	flagSet.StringVarP(&pflags.root, "root", "R", "", "Directory of the files to send (default: the current directory)")
	flagSet.StringVar(&pflags.changelog, "changelog-file", "", "Changelog to send with the version")
	flagSet.StringVar((*string)(&pflags.ifExists), "if-exists", "", "When the version is already published: fail, or skip to succeed without sending anything")
	flagSet.IntVar(&pflags.parallel, "parallel", 4, "Number of files sent at the same time")
	flagSet.BoolVar(&pflags.latest, "latest", false, "Also make this version the latest one of the product")
	flagSet.BoolVar(&pflags.isNotSemverStandard, "no-semver-standard", false, "Accept a version that is not X.Y.Z or X.Y.Z-pre, such as v1.2.0")
	flagSet.BoolVar(&pflags.dryRun, "dry-run", false, "Show what would be sent, without sending anything (no token needed)")
	flagSet.BoolVar(&pflags.noValidate, "no-validate", false, "Skip the checks of the product card and of artifacts.json")
	flagSet.StringArrayVar(&pflags.excludes, "exclude", nil, "File of the root directory not to send, such as '*.log' (repeatable)")
	flagSet.StringVar(&pflags.goReleaserArtifactsFile, "goreleaser-artifacts-file", "", "artifacts.json of a goreleaser build")
	flagSet.StringVar(&pflags.goReleaserMetadataFile, "goreleaser-metadata-file", "", "metadata.json of a goreleaser build")
	flagSet.StringVar(&pflags.goReleaserDir, "goreleaser-dir", "", "Directory of a goreleaser build (dist), with its artifacts.json and metadata.json")

	flagSet.BoolVar(&pflags.skipSubdirectories, "skip-subdirs", false, "Skip the sub directories of the root directory")
	flagSet.BoolVar(&pflags.compressSubdirectories, "compress-subdirs", false, "Send each sub directory of the root directory as one archive")
	flagSet.StringVar(&pflags.compressSubdirType, "compress-type", "tar.gz", "Archive format of --compress-subdirs: tar.gz or zip")

	_ = cmd.MarkFlagRequired("product-file")
}

func (f *publishFlags) validateProductCard(ctx context.Context, vCmd *validateCmd) error {
	if f.noValidate {
		return nil
	}
	problems, err := vCmd.validateProductFile(ctx, f.productFile)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		b := strings.Builder{}
		_, _ = fmt.Fprintf(&b, "the product card %s is not valid:", f.productFile)
		for _, p := range problems {
			_, _ = fmt.Fprintf(&b, "\n  %s: %s", p.Key, p.Message)
		}
		return config.Usage(errors.New(b.String()))
	}
	return nil
}

func (f *publishFlags) validateGoReleaseFile(ctx context.Context, vCmd *validateCmd, file string) error {
	if f.noValidate {
		return nil
	}
	problems := vCmd.validateArtifacts(ctx, file)
	if len(problems) > 0 {
		b := strings.Builder{}
		_, _ = fmt.Fprintf(&b, "%s is not valid:", file)
		for _, p := range problems {
			_, _ = fmt.Fprintf(&b, "\n  %s: %s", p.Key, p.Message)
		}
		return config.Usage(errors.New(b.String()))
	}
	return nil
}

func (f *publishFlags) safeRoot() string {
	if f.root == "" {
		return "."
	}
	return f.root
}

func (p *publishCmd) isGoReleaserBuilds() bool {
	return p.pFlags.goReleaserDir != "" || p.pFlags.goReleaserArtifactsFile != ""
}

// fileExists checks that path, after ~ is expanded, is a file; what names it in the error.
func fileExists(what, path string) error {
	p, err := config.ExpandPath(path)
	if err != nil {
		return fmt.Errorf("the %s file %s: %w", what, path, err)
	}
	info, err := os.Stat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("the %s file %s does not exist", what, path)
	case err != nil:
		return fmt.Errorf("the %s file %s cannot be read: %w", what, path, err)
	case info.IsDir():
		return fmt.Errorf("the %s file %s is a directory", what, path)
	}
	return nil
}

func (f *publishFlags) validateFlags(pName string, logger *slog.Logger, ctx context.Context, vCmd *validateCmd) error {
	if f.ifExists != "" && f.ifExists != fail && f.ifExists != skip {
		return config.Usage(fmt.Errorf("--if-exists must be fail or skip, not %q", f.ifExists))
	}

	if f.parallel < 1 {
		return config.Usage(fmt.Errorf("--parallel must be 1 or more, not %d", f.parallel))
	}

	if f.noValidate {
		logger.WarnContext(ctx, "--no-validate: the product card and artifacts.json are not checked before they are sent")
	}

	if f.changelog != "" {
		if err := fileExists("changelog", f.changelog); err != nil {
			return config.Usage(err)
		}
	}

	if f.productFile == "" {
		return config.Usage(errors.New("the product card is required: pass --product-file"))
	}

	if f.productFile != "" {
		if err := fileExists("product card", f.productFile); err != nil {
			return err
		}
		errp := f.validateProductCard(ctx, vCmd)
		if errp != nil {
			return errp
		}
	}

	if f.root != "" {
		exist, err := dirExists(f.root)
		if err != nil {
			return config.Usage(err)
		}
		if !exist {
			return config.Usage(fmt.Errorf("--root %s is not a directory", f.root))
		}
	}

	if f.compressSubdirType != "" {
		available := []string{"zip", "tar.gz"}
		if !slices.Contains(available, strings.ToLower(f.compressSubdirType)) {
			return config.Usage(fmt.Errorf("--compress-type must be %s, not %q", strings.Join(available, " or "), f.compressSubdirType))
		}
	}

	// Without --version, the version comes from the metadata.json of a goreleaser build:
	// processPublish checks that it has one.
	if f.version != "" && !f.isNotSemverStandard {
		// 	[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "--version $version is not X.Y.Z or X.Y.Z-pre"
		regx := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
		if !regx.Match([]byte(f.version)) {
			return config.Usage(fmt.Errorf("--version %s is not X.Y.Z or X.Y.Z-pre, such as 1.2.0-rc.1: pass --no-semver-standard to accept it", f.version))
		}
	}

	if f.goReleaserDir == "" && f.goReleaserArtifactsFile != "" {
		if err := fileExists("goreleaser artifacts", f.goReleaserArtifactsFile); err != nil {
			return config.Usage(err)
		}
		errp := f.validateGoReleaseFile(ctx, vCmd, f.goReleaserArtifactsFile)
		if errp != nil {
			return errp
		}
	}

	if f.goReleaserDir == "" && f.goReleaserMetadataFile != "" {
		if err := fileExists("goreleaser metadata", f.goReleaserMetadataFile); err != nil {
			return err
		}
	}

	if f.goReleaserDir != "" {
		if f.goReleaserArtifactsFile != "" {
			logger.Warn("--goreleaser-artifacts-file is ignored: --goreleaser-dir is set")
		}
		if f.goReleaserMetadataFile != "" {
			logger.Warn("--goreleaser-metadata-file is ignored: --goreleaser-dir is set")
		}

		exist, err := dirExists(f.goReleaserDir)
		if err != nil {
			return fmt.Errorf("--goreleaser-dir: %w", err)
		}
		if !exist {
			return fmt.Errorf("--goreleaser-dir %s is not a directory", f.goReleaserDir)
		}

		if fileExists("", f.goReleaserDir+"/artifacts.json") != nil {
			return fmt.Errorf("--goreleaser-dir: no artifacts.json in %s", f.goReleaserDir)
		}
		if fileExists("", f.goReleaserDir+"/metadata.json") != nil {
			return fmt.Errorf("--goreleaser-dir: no metadata.json in %s", f.goReleaserDir)
		}

		errp := f.validateGoReleaseFile(ctx, vCmd, fmt.Sprintf("%s/artifacts.json", f.goReleaserDir))
		if errp != nil {
			return errp
		}
	}

	return nil
}

// postValidateFlags switches to a goreleaser build when the root directory holds an
// artifacts.json. An artifacts.json that is not valid fails: it is not sent as a plain file.
func (f *publishFlags) postValidateFlags(logger *slog.Logger, ctx context.Context, vCmd *validateCmd) error {
	root := f.safeRoot()
	if f.goReleaserArtifactsFile == "" && f.goReleaserDir == "" {
		af := fmt.Sprintf("%s/artifacts.json", root)
		if fileExists("", af) == nil {
			logger.InfoContext(ctx, "artifacts.json found in the root directory")
			if err := f.validateGoReleaseFile(ctx, vCmd, af); err != nil {
				return fmt.Errorf("the root directory holds an artifacts.json: %w", err)
			}
			f.goReleaserArtifactsFile = af
			f.goReleaserDir = root
			if f.goReleaserMetadataFile == "" {
				mf := fmt.Sprintf("%s/metadata.json", root)
				if fileExists("", mf) == nil {
					logger.InfoContext(ctx, "metadata.json found in the root directory")
					f.goReleaserMetadataFile = mf
				}
			}
		}
	}

	if f.goReleaserDir != "" {
		f.goReleaserArtifactsFile = fmt.Sprintf("%s/artifacts.json", f.goReleaserDir)
		f.goReleaserMetadataFile = fmt.Sprintf("%s/metadata.json", f.goReleaserDir)
	}
	return nil
}

func dirExists(path string) (bool, error) {
	p, _ := config.ExpandPath(path)
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("the directory %s does not exist", path)
		}
		return false, err
	}

	return info.IsDir(), nil
}

// put uploads the file at path under name, the file name on the server.
func (p *publishCmd) put(ctx context.Context, no, total int, name, path string) error {
	checksum, err := util.Sha256File(path)
	if err != nil {
		return err
	}
	p.log.DebugContext(ctx, "PUT", "no", no, "name", name, "path", path, "checksum", checksum)
	if p.pFlags.dryRun {
		// Nothing is sent: count the size that would be.
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		p.totalSize += info.Size()
		_, _ = fmt.Fprintf(p.printer.Err, "[%d/%d] %s  %s  sha256 %s (not sent: dry run)\n", no, total, name,
			humanBytes(info.Size()), checksum)
		return nil
	}
	return p.httpUpload(ctx, fmt.Sprintf("[%d/%d] %s", no, total, name), name, path, checksum)
}

// httpUpload sends the file at path with a PUT to {product}/artifacts/{version}/files/{name},
// as a stream, and draws its progress on stderr under label. The server checks the file
// against checksum, its sha256.
func (p *publishCmd) httpUpload(ctx context.Context, label, name, path, checksum string) error {
	token, err := p.uploadToken()
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf(productUploadEndpoint, p.pName, p.pFlags.version, name)
	progress := newProgressBar(p.printer.Err, label, p.printer.UseColor())

	resp, err := p.cfg.HttpClient().Put(ctx, endpoint, &httpclient.RequestOptions{
		Headers: []httpclient.OptionParamItem{
			httpclient.NewRequestOption("Authorization", "Bearer "+token),
			httpclient.NewRequestOption("X-Checksum-Sha256", checksum),
		},
		Body:           f,
		ContentLength:  info.Size(),
		UploadProgress: progress.Update,
	})
	if err != nil {
		progress.Fail()
		return fmt.Errorf("upload %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !httpclient.IsSuccess(resp) {
		refusal := serverRefusal(resp)
		if p.pFlags.ifExists == skip && errors.Is(refusal, errAlreadyPublished) {
			progress.Skip()
		} else {
			progress.Fail()
		}
		return fmt.Errorf("upload %s: %w", name, refusal)
	}
	p.totalSize += info.Size()
	progress.Done()
	return nil
}

// uploadToken returns the upload token, needed to upload a file and to publish a version.
func (p *publishCmd) uploadToken() (string, error) {
	if p.cfg.Server.UploadToken == "" {
		return "", errors.New("no upload token: set LOCAL_REGISTRY_UPLOAD_TOKEN or server.upload-token in the config file")
	}
	return p.cfg.Server.UploadToken, nil
}

// publishedVersion is the answer of the server to a publish.
type publishedVersion struct {
	Product string   `json:"product"`
	Version string   `json:"version"`
	Files   []string `json:"files"`
	Latest  bool     `json:"latest"`
}

// httpPublishCall publishes the uploaded files of the version with a POST to
// {product}/artifacts/{version}/publish: the server checks them, then makes the version public,
// and the latest one with --latest.
func (p *publishCmd) httpPublishCall(ctx context.Context) error {
	token, err := p.uploadToken()
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf(productPublishEndpoint, p.pName, p.pFlags.version)
	opts := &httpclient.RequestOptions{
		QueryParams: []httpclient.OptionParamItem{
			httpclient.NewRequestOption("latest", strconv.FormatBool(p.pFlags.latest)),
		},
		Headers: []httpclient.OptionParamItem{
			httpclient.NewRequestOption("Authorization", "Bearer "+token),
		},
		// Publishing is not idempotent: it is retried here, to know when an attempt is a retry.
		NoRetry: true,
	}
	var resp *http.Response
	attempt := 0
	for ; ; attempt++ {
		resp, err = p.cfg.HttpClient().Post(ctx, endpoint, opts)
		var ce *httpclient.ClientError
		if err == nil || !errors.As(err, &ce) || !ce.Retryable() || ctx.Err() != nil || attempt >= p.cfg.Retries {
			break
		}
		p.log.WarnContext(ctx, "publish failed, retried", "attempt", attempt+1, "error", err)
		select {
		case <-time.After(time.Second << attempt):
		case <-ctx.Done():
			return fmt.Errorf("publish %s %s: %w", p.pName, p.pFlags.version, ctx.Err())
		}
	}
	if err != nil {
		return fmt.Errorf("publish %s %s: %w", p.pName, p.pFlags.version, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !httpclient.IsSuccess(resp) {
		refusal := serverRefusal(resp)
		// The answer of a previous attempt may have been lost after the server published.
		if attempt > 0 && errors.Is(refusal, errAlreadyPublished) {
			if published, err := p.isPublished(ctx); err == nil && published {
				p.log.InfoContext(ctx, "version published by a previous attempt", "product", p.pName, "version", p.pFlags.version)
				return nil
			}
		}
		return fmt.Errorf("publish %s %s: %w", p.pName, p.pFlags.version, refusal)
	}
	var published publishedVersion
	if err := json.NewDecoder(resp.Body).Decode(&published); err != nil {
		return fmt.Errorf("publish %s %s: the response is not a published version: %w", p.pName, p.pFlags.version, err)
	}
	p.log.Info("version published", "product", published.Product, "version", published.Version,
		"latest", published.Latest, "files", len(published.Files))
	return nil
}

// errAlreadyPublished tells that the version is already published: a published version never
// changes.
var errAlreadyPublished = errors.New("the version is already published")

// serverRefusal explains a refused upload or publish, with the {"error": "..."} message of the
// server. A 409 wraps errAlreadyPublished.
func serverRefusal(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	msg := resp.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err == nil && body.Error != "" {
		msg = body.Error
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("the upload token is missing or invalid: " + msg)
	case http.StatusForbidden:
		return errors.New("refused by the server: " + msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", errAlreadyPublished, msg)
	}
	return errors.New(msg)
}

// sendsTokenInClear tells whether the token goes over plain http to another machine: http to
// localhost or a loopback address stays on this machine.
func sendsTokenInClear(serverURL string) bool {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// isPublished tells whether the version is already published, with a GET of its release.json:
// 200 when it is, 404 when it is not (nor the product, before its first version). It needs no
// token.
func (p *publishCmd) isPublished(ctx context.Context) (bool, error) {
	endpoint := fmt.Sprintf(productReleaseEndpoint, p.pName, p.pFlags.version)
	resp, err := p.cfg.HttpClient().Get(ctx, endpoint, nil)
	if err != nil {
		// A network error comes as a fake 500: its message is the cause.
		var ce *httpclient.ClientError
		if errors.As(err, &ce) && ce.Message != "" {
			err = errors.New(ce.Message)
		}
		return false, fmt.Errorf("check whether %s %s is published: %w", p.pName, p.pFlags.version, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case httpclient.IsSuccess(resp):
		return true, nil
	}
	return false, fmt.Errorf("check whether %s %s is published: %s", p.pName, p.pFlags.version, resp.Status)
}

// skipped is the summary of --if-exists skip on a published version: sent counts the files sent
// before the server answered "already published".
func (p *publishCmd) skipped(ctx context.Context, sent int, reason error) *summary {
	p.log.InfoContext(ctx, "the version is already published: nothing more sent (--if-exists skip)",
		"product", p.pName, "version", p.pFlags.version, "reason", reason)
	return &summary{Total: sent, TotalSize: p.totalSize, Status: "SKIPPED", Version: p.pFlags.version, Product: p.pName}
}

func (p *publishCmd) publish(ctx context.Context) error {
	// echo "POST publish $product/$version (latest=$latest)"
	p.log.DebugContext(ctx, "POST publish", "product", p.pName, "version", p.pFlags.version, "latest", p.pFlags.latest)
	if p.pFlags.dryRun {
		return nil
	}
	return p.httpPublishCall(ctx)
}

func (p *publishCmd) processPublish(cmd *cobra.Command) (*summary, error) {
	ctx := cmd.Context()
	root := p.pFlags.safeRoot()
	var files []string
	var err error
	if p.isGoReleaserBuilds() {
		var versions []string
		files, versions, err = p.readFilesFromGoReleaser(root)
		if err != nil {
			return nil, err
		}
		// --version must be the tag or the version of metadata.json; without it, the tag is
		// taken first, as upload.sh does.
		switch {
		case len(versions) == 0:
		case p.pFlags.version == "":
			p.pFlags.version = versions[0]
		case !slices.Contains(versions, p.pFlags.version):
			return nil, fmt.Errorf("--version %s is not the tag nor the version of metadata.json (%s)",
				p.pFlags.version, strings.Join(versions, ", "))
		}
	} else {
		files, err = p.readFilesFromRootDir(root)
	}
	if err != nil {
		return nil, err
	}
	if p.pFlags.version == "" {
		return nil, errors.New("the version is required: pass --version, or publish a goreleaser build with a metadata.json")
	}
	if err := pathSegmentArg("version", p.pFlags.version); err != nil {
		return nil, err
	}

	// Check before anything is sent, so a published version costs no upload. Even in dry run:
	// the check only reads.
	published, err := p.isPublished(ctx)
	if err != nil {
		return nil, err
	}
	if published {
		if p.pFlags.ifExists == skip {
			return p.skipped(ctx, 0, errAlreadyPublished), nil
		}
		return nil, fmt.Errorf("%s %s: %w: pass --if-exists skip to succeed without sending anything",
			p.pName, p.pFlags.version, errAlreadyPublished)
	}

	// The product card may live outside the root (the current directory by default): its path
	// is the one given, and the server always names it registry.yaml.
	productFile, err := config.ExpandPath(p.pFlags.productFile)
	if err != nil {
		return nil, err
	}

	uploads, err := p.uploads(files, productFile)
	if err != nil {
		return nil, err
	}
	if !p.pFlags.dryRun && sendsTokenInClear(p.cfg.Server.BaseUrl) {
		p.log.WarnContext(ctx, "the upload token is sent in clear: server.url is not https", "url", p.cfg.Server.BaseUrl)
	}

	// Another job may publish the version while this one sends it: with --if-exists skip, the
	// "already published" (409) of the server is a skip too.
	size := len(uploads)
	for i, u := range uploads {
		if err = p.put(ctx, i+1, size, u.name, u.path); err != nil {
			if p.pFlags.ifExists == skip && errors.Is(err, errAlreadyPublished) {
				return p.skipped(ctx, i, err), nil
			}
			return nil, err
		}
	}

	if err = p.publish(ctx); err != nil {
		if p.pFlags.ifExists == skip && errors.Is(err, errAlreadyPublished) {
			return p.skipped(ctx, size, err), nil
		}
		return nil, err
	}

	status := "PUBLISHED"
	if p.pFlags.dryRun {
		status = "DRY-RUN"
	}
	sum := summary{
		Total:     size,
		TotalSize: p.totalSize,
		Status:    status,
		Version:   p.pFlags.version,
		Product:   p.pName,
	}

	return &sum, nil
}

// upload is a file to send: path on this machine, name on the server.
type upload struct {
	name, path string
}

// uploads lists the files to send, in order: files, then the product card as registry.yaml, and
// the --changelog-file as CHANGELOG.md, the names the server reads them by (as upload.sh). A
// file of files with one of these names is skipped: the one given explicitly wins.
func (p *publishCmd) uploads(files []string, productFile string) ([]upload, error) {
	explicit := []upload{{productCardName, productFile}}
	if p.pFlags.changelog != "" {
		changelog, err := config.ExpandPath(p.pFlags.changelog)
		if err != nil {
			return nil, fmt.Errorf("--changelog-file: %w", err)
		}
		explicit = append(explicit, upload{changelogName, changelog})
	}

	list := make([]upload, 0, len(files)+len(explicit))
	for _, file := range files {
		name := filepath.Base(file)
		if slices.ContainsFunc(explicit, func(u upload) bool { return u.name == name }) {
			p.log.Warn("file skipped: its name is the one of a file given by an option", "file", file, "name", name)
			continue
		}
		list = append(list, upload{name, file})
	}
	return append(list, explicit...), nil
}

func (p *publishCmd) readFilesFromRootDir(rootDir string) ([]string, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}

	var files []string
	var walkErrors []error

	for _, entry := range entries {
		// A hidden file (.env, .git, .DS_Store) may hold a secret: never sent.
		if strings.HasPrefix(entry.Name(), ".") {
			p.log.Info("hidden file skipped", "file", entry.Name())
			continue
		}

		if entry.IsDir() {
			// normalizePath only resolves regular files: a directory keeps its raw path.
			path := filepath.Join(rootDir, entry.Name())
			if p.pFlags.skipSubdirectories {
				p.log.Warn("sub directory skipped", "dir", entry.Name())
				continue
			} else if p.pFlags.compressSubdirectories {
				archive, err := p.compressDir(path, entry.Name())
				if err != nil {
					walkErrors = append(walkErrors, fmt.Errorf("compress %s: %w", path, err))
					continue
				}
				files = append(files, archive)
				continue
			} else {
				walkErrors = append(walkErrors,
					fmt.Errorf("%s is a directory: archive it first (tar czf %s.tar.gz -C %s %s), or pass --compress-subdirs or --skip-subdirs",
						path, entry.Name(), rootDir, entry.Name()))
				continue
			}
		}

		if slices.Contains(reservedFilenames, strings.ToLower(entry.Name())) {
			return nil, fmt.Errorf("%s is a reserved name: the server writes it on publish (reserved: %s)", entry.Name(),
				strings.Join(reservedFilenames, ", "))
		}

		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			p.log.Warn("not a regular file, skipped", "file", entry.Name())
			continue
		}

		path, err := p.normalizePath(rootDir, filepath.Join(rootDir, entry.Name()))
		if err != nil {
			p.log.Warn("file skipped", "file", entry.Name(), "error", err)
			continue
		}
		files = append(files, path)
	}
	return files, errors.Join(walkErrors...)
}

// compressDir archives the directory at path as name.tar.gz or name.zip (--compress-type), in a
// temporary directory created at the first archive and removed by tmpCleanup.
func (p *publishCmd) compressDir(path, name string) (string, error) {
	if p.tmpDir == "" {
		tmp, err := util.CreateSoftTempdir()
		if err != nil {
			return "", err
		}
		p.addTmpPath(tmp)
		p.tmpDir = tmp
	}
	ext := "." + strings.ToLower(p.pFlags.compressSubdirType)
	archive := filepath.Join(p.tmpDir, name+ext)
	compress := util.TarGzDir
	if ext == ".zip" {
		compress = util.ZipDir
	}
	if err := compress(path, archive); err != nil {
		return "", err
	}
	return archive, nil
}

// readFilesFromGoReleaser lists the files of a goreleaser build: the files of artifacts.json
// whose type is sent, then artifacts.json and metadata.json. The paths of artifacts.json are
// relative to --goreleaser-dir, else to root. versions holds the tag, then the version, of
// metadata.json, when it has them.
func (p *publishCmd) readFilesFromGoReleaser(root string) (files, versions []string, err error) {
	base := p.pFlags.goReleaserDir
	if base == "" {
		base = root
	}

	artifactsFile, err := config.ExpandPath(p.pFlags.goReleaserArtifactsFile)
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(artifactsFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read the goreleaser artifacts: %w", err)
	}
	var artifacts []artifact
	if err = json.Unmarshal(b, &artifacts); err != nil {
		return nil, nil, fmt.Errorf("%s is not a valid artifacts.json: %w", artifactsFile, err)
	}

	for _, a := range artifacts {
		if !slices.Contains(artifactSupportedFileTypes, a.Type) {
			continue
		}
		path, err := p.normalizePath(base, a.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", artifactsFile, err)
		}
		files = append(files, path)
	}
	files = append(files, artifactsFile)

	if p.pFlags.goReleaserMetadataFile != "" && fileExists("", p.pFlags.goReleaserMetadataFile) == nil {
		metadataFile, err := config.ExpandPath(p.pFlags.goReleaserMetadataFile)
		if err != nil {
			return nil, nil, err
		}
		m, err := os.ReadFile(metadataFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read the goreleaser metadata: %w", err)
		}
		var metadata struct {
			Tag     string `json:"tag"`
			Version string `json:"version"`
		}
		if err = json.Unmarshal(m, &metadata); err != nil {
			return nil, nil, fmt.Errorf("%s is not a valid metadata.json: %w", metadataFile, err)
		}
		for _, v := range []string{metadata.Tag, metadata.Version} {
			if v != "" {
				versions = append(versions, v)
			}
		}
		files = append(files, metadataFile)
	}

	return files, versions, nil
}

// normalizePath resolves path under rootDir: a regular file, neither a symbolic link nor out of
// rootDir (see util.ResolveEntry).
func (p *publishCmd) normalizePath(rootDir, path string) (string, error) {
	resolved, err := util.ResolveEntry(rootDir, path)
	if err != nil {
		return "", err
	}
	return resolved.Path, nil
}
