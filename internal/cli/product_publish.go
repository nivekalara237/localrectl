package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

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

	totalSize int64 // bytes uploaded so far
}

type summary struct {
	Total     int    `json:"total"`
	TotalSize int64  `json:"total_size"` // bytes uploaded
	Status    string `json:"status"`
	Version   string `json:"version"`
	Product   string `json:"product"`
}

type publishFlags struct {
	/*Fait aussi de cette version la latest du produit*/
	latest bool
	/*Affiche ce qui serait envoyé, sans rien envoyer (pas besoin de token)*/
	dryRun bool
	/*Dossier auquel les chemins d'artifacts.json sont relatifs*/
	root string
	/*Version à publier. Par défaut : tag, sinon version, de metadata.json*/
	version             string
	isNotSemverStandard bool
	/*La fiche produit registry.yaml. Par défaut, celle de la racine, sinon celle du dossier courant*/
	productFile string

	/*CHANGELOG.md à envoyer. Par défaut, celui qui est à côté d'artifacts.json*/
	changelog string
	/*Fichiers d'un DIR à ne pas envoyer (*.log, .DS_Store)*/
	excludes []string
	/*Envois en parallèle (4 par défaut)*/
	parallel int
	/*Saute la validation locale, faite par défaut avant tout envoi*/
	noValidate bool
	/*Que faire si la version existe déjà : échec (par défaut), ou succès sans rien faire, pour rejouer une CI sans erreur*/
	ifExists ifExists

	// for GoReleaser
	goReleaserArtifactsFile string
	goReleaserMetadataFile  string
	goReleaserDir           string // if set, not need goReleaserArtifactsFile and goReleaserMetadataFile

	// directory walking
	skipSubdirectories     bool
	compressSubdirectories bool
	compressSubdirType     string
}
type ifExists string

var (
	fail ifExists = "fail"
	skip ifExists = "skip"
)

var (
	productPublishEndpoint = "/products/publish"
)

var reservedFilenames = []string{
	"index.json",
	"release.json",
	"artifacts.goreleaser.json",
}
var artifactSupportedFileTypes = []string{"Archive", "File", "Source", "Linux Package", "Checksum",
	"Signature", "Certificate", "SBOM", "Metadata"}

var pflags publishFlags

// tmpPaths holds the temporary files and directories created while the command runs. They stay
// on disk until the upload is done, then tmpCleanup removes them.
var tmpPaths []string

// addTmpPath records a temporary file or directory for tmpCleanup.
func addTmpPath(path string) {
	tmpPaths = append(tmpPaths, path)
}

// tmpCleanup removes every temporary file and directory recorded by addTmpPath.
func tmpCleanup(log *slog.Logger) {
	for _, path := range tmpPaths {
		if err := os.RemoveAll(path); err != nil {
			log.Warn("unable to remove a temporary path", "path", path, "error", err)
		}
	}
	tmpPaths = nil
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
	cmd := cobra.Command{
		Use:     "publish",
		Short:   "Publish a version of the product",
		Long:    "",
		Example: "",
		Aliases: []string{"product-publish"},
		Args:    withArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logging.From(cmd.Context())
			defer tmpCleanup(p.Logger())

			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("unable to load config")
			}

			pname := ""
			if len(args) > 0 {
				pname = args[0]
			}

			validationCmd := newValidateCmd(cfg, p.Logger())

			err := pflags.validateFlags(pname, p.Logger(), cmd.Context(), validationCmd)
			if err != nil {
				return err
			}
			pflags.postValidateFlags(p.Logger(), cmd.Context(), validationCmd)

			pcmd := newPublishCmd(pname, p, cfg, pflags)
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

	setFlagSet(&cmd)

	return &cmd
}

func setFlagSet(cmd *cobra.Command) {
	flagSet := cmd.Flags()
	flagSet.StringVar(&pflags.version, "version", "", "")
	flagSet.StringVar(&pflags.productFile, "product-file", "", "")
	flagSet.StringVarP(&pflags.root, "root", "R", "", "")
	flagSet.StringVar(&pflags.changelog, "changelog-file", "", "")
	flagSet.StringVar((*string)(&pflags.ifExists), "if-exists", "", "")
	flagSet.IntVar(&pflags.parallel, "parallel", 4, "")
	flagSet.BoolVar(&pflags.latest, "latest", false, "")
	flagSet.BoolVar(&pflags.isNotSemverStandard, "no-semver-standard", false, "")
	flagSet.BoolVar(&pflags.dryRun, "dry-run", false, "")
	flagSet.BoolVar(&pflags.noValidate, "no-validate", false, "")
	flagSet.StringArrayVar(&pflags.excludes, "exclude", nil, "")
	flagSet.StringVar(&pflags.goReleaserArtifactsFile, "goreleaser-artifacts-file", "", "")
	flagSet.StringVar(&pflags.goReleaserMetadataFile, "goreleaser-metadata-file", "", "")
	flagSet.StringVar(&pflags.goReleaserDir, "goreleaser-dir", "", "")

	flagSet.BoolVar(&pflags.skipSubdirectories, "skip-subdirs", false, "")
	flagSet.BoolVar(&pflags.compressSubdirectories, "compress-subdirs", false, "")
	flagSet.StringVar(&pflags.compressSubdirType, "compress-type", "tar.gz", "")

	_ = cmd.MarkFlagRequired("product-file")
	_ = cmd.MarkFlagRequired("version")
}

func (f *publishFlags) validateProductCard(ctx context.Context, vCmd *validateCmd) error {
	problems, err := vCmd.validateProductFile(ctx, f.productFile)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		b := strings.Builder{}
		for _, p := range problems {
			b.WriteString(fmt.Sprintf("%s: %s\n", p.Key, p.Message))
		}
		return errors.New(b.String())
	}
	return nil
}

func (f *publishFlags) validateGoReleaseFile(ctx context.Context, vCmd *validateCmd, file string) error {
	problems := vCmd.validateArtifacts(ctx, file)
	if len(problems) > 0 {
		b := strings.Builder{}
		for _, p := range problems {
			b.WriteString(fmt.Sprintf("%s: %s\n", p.Key, p.Message))
		}
		return errors.New(b.String())
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

func fileExists(path string) (bool, string) {
	p, _ := config.ExpandPath(path)
	_, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "the %s file: %s doesn't exist"
		}
		return false, err.Error()
	}
	return true, ""
}

func (f *publishFlags) validateFlags(pName string, logger *slog.Logger, ctx context.Context, vCmd *validateCmd) error {
	if f.ifExists != "" && (f.ifExists != fail || f.ifExists != skip) {
		return errors.New("--if-exists option must be skip or fail")
	}

	if f.parallel < 1 {
		return errors.New("the parallel option must be eager than 1")
	}

	if f.changelog != "" {
		ok, errf := fileExists(f.changelog)
		if !ok {
			return fmt.Errorf(errf, "changelog", f.changelog)
		}
	}

	if f.productFile == "" {
		return errors.New("the product card is required")
	}

	if f.productFile != "" {
		ok, errf := fileExists(f.productFile)
		if !ok {
			return fmt.Errorf(errf, "product card", f.productFile)
		}
		errp := f.validateProductCard(ctx, vCmd)
		if errp != nil {
			return errp
		}
	}

	if f.root != "" {
		exist, err := dirExists(f.root)
		if err != nil {
			return err
		}
		if !exist {
			return errors.New(f.root + " not exists")
		}
	}

	if f.compressSubdirType != "" {
		available := []string{"zip", "tar.gz"}
		if !slices.Contains(available, strings.ToLower(f.compressSubdirType)) {
			return fmt.Errorf("compress type must one of %s", strings.Join(available, ","))
		}
	}

	if f.version == "" {
		return errors.New("the version (--version) is required")
	}

	if f.version != "" && !f.isNotSemverStandard {
		// 	[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "--version $version is not X.Y.Z or X.Y.Z-pre"
		regx := regexp.MustCompile("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$")
		if !regx.Match([]byte(f.version)) {
			return errors.New("--version is not X.Y.Z or X.Y.Z-alpha.1")
		}
	}

	if f.goReleaserDir == "" && f.goReleaserArtifactsFile != "" {
		ok, errf := fileExists(f.goReleaserArtifactsFile)
		if !ok {
			return fmt.Errorf(errf, "GoReleaser artifacts", f.goReleaserArtifactsFile)
		}
		errp := f.validateGoReleaseFile(ctx, vCmd, f.goReleaserArtifactsFile)
		if errp != nil {
			return errp
		}
	}

	if f.goReleaserDir == "" && f.goReleaserMetadataFile != "" {
		ok, errf := fileExists(f.goReleaserMetadataFile)
		if !ok {
			return fmt.Errorf(errf, "GoReleaser metadata", f.goReleaserMetadataFile)
		}
	}

	if f.goReleaserDir != "" {
		if f.goReleaserArtifactsFile != "" {
			logger.Warn("GoReleaser: --goreleaser-artifacts-file will be ignored as --goreleaser-dir is set")
		}
		if f.goReleaserMetadataFile != "" {
			logger.Warn("GoReleaser: --goreleaser-metadata-file will be ignored as --goreleaser-dir is set")
		}

		exist, err := dirExists(f.goReleaserDir)
		if err != nil {
			return fmt.Errorf("GoReleaser: %w", err)
		}
		if !exist {
			return fmt.Errorf("GoReleaser: %s not exists", f.goReleaserDir)
		}

		ok, _ := fileExists(f.goReleaserDir + "/artifacts.json")
		if !ok {
			return fmt.Errorf("GoReleaser: %s/artifacts.json no found", f.goReleaserDir)
		}
		ok, _ = fileExists(f.goReleaserDir + "/metadata.json")
		if !ok {
			return fmt.Errorf("GoReleaser: %s/metadata.json no found", f.goReleaserDir)
		}

		errp := f.validateGoReleaseFile(ctx, vCmd, fmt.Sprintf("%s/artifacts.json", f.goReleaserDir))
		if errp != nil {
			return errp
		}
	}

	return nil
}

func (f *publishFlags) postValidateFlags(logger *slog.Logger, ctx context.Context, vCmd *validateCmd) {
	root := f.safeRoot()
	if f.goReleaserArtifactsFile == "" && f.goReleaserDir == "" {
		af := fmt.Sprintf("%s/artifacts.json", root)
		artifactsExist, _ := fileExists(af)
		if artifactsExist {
			logger.InfoContext(ctx, "artifacts.json found in the root directory")
			err := f.validateGoReleaseFile(ctx, vCmd, af)
			if err != nil {
				logger.ErrorContext(ctx, "root/artifacts", "permissive", "true", "error", err)
			} else {
				f.goReleaserArtifactsFile = af
			}
			f.goReleaserDir = root
			if f.goReleaserMetadataFile == "" {
				mf := fmt.Sprintf("%s/metadata.json", root)
				medataExist, _ := fileExists(mf)
				if medataExist {
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
}

func dirExists(path string) (bool, error) {
	p, _ := config.ExpandPath(path)
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("'%s' directory doesn't exists", path)
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
	p.log.Debug("PUT", "no", no, "name", name, "path", path, "checksum", checksum)
	if p.pFlags.dryRun {
		// Nothing is sent: count the size that would be.
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		p.totalSize += info.Size()
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

	endpoint := fmt.Sprintf("%s/artifacts/%s/files/%s", p.pName, p.pFlags.version, name)
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
		progress.Fail()
		return fmt.Errorf("upload %s: %s", name, serverRefusal(resp))
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

	endpoint := fmt.Sprintf("%s/artifacts/%s/publish", p.pName, p.pFlags.version)
	resp, err := p.cfg.HttpClient().Post(ctx, endpoint, &httpclient.RequestOptions{
		QueryParams: []httpclient.OptionParamItem{
			httpclient.NewRequestOption("latest", strconv.FormatBool(p.pFlags.latest)),
		},
		Headers: []httpclient.OptionParamItem{
			httpclient.NewRequestOption("Authorization", "Bearer "+token),
		},
	})
	if err != nil {
		return fmt.Errorf("publish %s %s: %w", p.pName, p.pFlags.version, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !httpclient.IsSuccess(resp) {
		return fmt.Errorf("publish %s %s: %s", p.pName, p.pFlags.version, serverRefusal(resp))
	}
	var published publishedVersion
	if err := json.NewDecoder(resp.Body).Decode(&published); err != nil {
		return fmt.Errorf("publish %s %s: the response is not a published version: %w", p.pName, p.pFlags.version, err)
	}
	p.log.Info("version published", "product", published.Product, "version", published.Version,
		"latest", published.Latest, "files", len(published.Files))
	return nil
}

// serverRefusal explains a refused upload or publish, with the {"error": "..."} message of the
// server.
func serverRefusal(resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	msg := resp.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err == nil && body.Error != "" {
		msg = body.Error
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return "the upload token is missing or invalid: " + msg
	case http.StatusForbidden:
		return "upload refused: " + msg
	case http.StatusConflict:
		return "the version is already published: " + msg
	}
	return msg
}

func (p *publishCmd) publish(ctx context.Context) error {
	// echo "POST publish $product/$version (latest=$latest)"
	p.log.Debug("POST publish", "product", p.pName, "version", p.pFlags.version, "latest", p.pFlags.latest)
	if p.pFlags.dryRun {
		return nil
	}
	return p.httpPublishCall(ctx)
}

func (p *publishCmd) processPublish(cmd *cobra.Command) (*summary, error) {
	root := pflags.safeRoot()
	var files []string
	var err error
	var grVersion string
	if p.isGoReleaserBuilds() {
		files, grVersion, err = p.readFilesFromGoReleaser(root)
		if p.pFlags.version != "" && p.pFlags.version != grVersion {
			return nil, errors.New("--version value and metadata#Version or metadata#Tag are different")
		}
		if p.pFlags.version == "" {
			p.pFlags.version = grVersion
		}
	} else {
		files, err = p.readFilesFromRootDir(root)
	}
	if err != nil {
		return nil, err
	}

	// The product card may live outside the root (the current directory by default): its path
	// is the one given, and the server always names it registry.yaml.
	productFile, err := config.ExpandPath(p.pFlags.productFile)
	if err != nil {
		return nil, err
	}

	size := len(files) + 1 // +1 for registry.yaml

	for i, file := range files {
		err = p.put(cmd.Context(), i+1, size, filepath.Base(file), file)
		if err != nil {
			return nil, err
		}
	}

	err = p.put(cmd.Context(), size, size, "registry.yaml", productFile)
	if err != nil {
		return nil, err
	}

	err = p.publish(cmd.Context())
	if err != nil {
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

func (p *publishCmd) readFilesFromRootDir(rootDir string) ([]string, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}
	tmp, err := util.CreateSoftTempdir()
	if err != nil {
		return nil, err
	}
	addTmpPath(tmp)

	var files []string
	var walkErrors []error

	for _, entry := range entries {
		if entry.IsDir() {
			// normalizePath only resolves regular files: a directory keeps its raw path.
			path := filepath.Join(rootDir, entry.Name())
			if pflags.skipSubdirectories {
				p.log.Warn("skipping sub directory", "dirname", entry.Name())
				continue
			} else if pflags.compressSubdirectories {
				ext := "." + strings.ToLower(pflags.compressSubdirType)
				compressedName := filepath.Join(tmp, entry.Name()+ext)
				compress := util.TarGzDir
				if ext == ".zip" {
					compress = util.ZipDir
				}
				if err := compress(path, compressedName); err != nil {
					walkErrors = append(walkErrors, fmt.Errorf("compress %s: %w", path, err))
					continue
				}
				files = append(files, compressedName)
				continue
			} else {
				walkErrors = append(walkErrors,
					fmt.Errorf("is a directory: archive it first, e.g. tar czf %s.tar.gz -C %s %s",
						entry.Name(), path, entry.Name()))
				continue
			}
		}

		if slices.Contains(reservedFilenames, strings.ToLower(entry.Name())) {
			return nil, fmt.Errorf("%s is a reserved file. (Reserved: %s)", entry.Name(),
				strings.Join(reservedFilenames, ", "))
		}

		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			p.log.Warn("irregular file", "file", entry.Name())
			continue
		}

		path := p.normalizePath(rootDir, filepath.Join(rootDir, entry.Name()))
		if path == "" {
			p.log.Warn("unresolved file", "file", entry.Name())
			continue
		}
		files = append(files, path)
	}
	return files, errors.Join(walkErrors...)
}

func (p *publishCmd) readFilesFromGoReleaser(root string) (files []string, version string, err error) {

	b, _ := os.ReadFile(p.normalizePath(p.pFlags.goReleaserDir, p.pFlags.goReleaserArtifactsFile))

	var artifacts []artifact
	if err = json.Unmarshal(b, &artifacts); err != nil {
		return nil, "", errors.New("unable to parse artifacts.json")
	}

	for _, a := range artifacts {
		if slices.Contains(artifactSupportedFileTypes, a.Type) {
			files = append(files, p.normalizePath(p.pFlags.goReleaserDir, a.Path))
		}
	}

	if ok, _ := fileExists(p.pFlags.goReleaserMetadataFile); ok {
		type meta struct {
			Tag     string `json:"tag"`
			Version string `json:"version"`
		}
		var metadata meta
		m, _ := os.ReadFile(p.pFlags.goReleaserMetadataFile)
		if err = json.Unmarshal(m, &metadata); err != nil {
			return nil, "", err
		}
		if metadata.Version != "" {
			version = metadata.Version
		} else if metadata.Tag != "" {
			version = metadata.Tag
		}
	}

	files = append(files, p.normalizePath(p.pFlags.goReleaserDir, p.pFlags.goReleaserArtifactsFile))
	if p.pFlags.goReleaserMetadataFile != "" {
		files = append(files, p.normalizePath(p.pFlags.goReleaserDir, p.pFlags.goReleaserMetadataFile))
	}

	return files, version, nil
}

func (p *publishCmd) normalizePath(rootDir, assetRelativePath string) string {
	resolved, err := util.ResolveEntry(rootDir, assetRelativePath)
	if err != nil {
		return ""
	}
	if !resolved.Info.Mode().IsRegular() {
		return ""
	}

	return resolved.Path
}
