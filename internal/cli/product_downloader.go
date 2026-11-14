package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

type downloaderCmd struct {
	cfg        *config.Config
	log        *slog.Logger
	printer    *logging.Printer
	pName, ver string
}

const (
	productSearchEndpoint = "/product/search"
	productDownload       = "/product/%s/artifacts/%s/files/%S"
)

var kinds = []string{
	"archive",
	"binary",
	"package",
	"file",
}

var oSs = []string{
	"windows",
	"linux",
	"macos",
}

var archs = []string{
	"amd64",
	"arm64",
}

type pdFlags struct {
	/*The operating system*/
	os string
	/*Architecture*/
	arch string
	/*Filtre par type de fichier*/
	kind string
	/*Fichiers précis, par nom. Répétable*/
	files []string

	/*Tous les fichiers de la version*/
	all bool

	/*Dossier de destination*/
	destDir string

	/*Décompresse l'archive àpres vérification*/
	extract bool

	/*Saute le controlé sha256. Déconseillé, puisque la spec dema,de au client de vérifier*/
	noVerify bool

	/*Vérifie la signature Sigstore du ficher de checksums, si `cosign` est installé*/
	verifySignature bool
}

var cmdflags pdFlags

func (f *pdFlags) validateFlags() error {

	if f.destDir != "" {
		if err := fileExists("destination dir", f.destDir); err != nil {
			return err
		}
	}

	if f.os != "" && slices.Contains(oSs, f.os) {
		return fmt.Errorf("the operating system type must be one of %s: , not %s", strings.Join(oSs, ","), f.os)
	}
	if f.arch != "" && slices.Contains(archs, f.arch) {
		return fmt.Errorf("the architecture type must be one of %s: , not %s", strings.Join(oSs, ","), f.arch)
	}

	return nil
}

func newDownloaderCmd(cfg *config.Config, p *logging.Printer, pName, version string) *downloaderCmd {
	return &downloaderCmd{
		cfg:     cfg,
		log:     p.Logger().With("cmd", "downloader"),
		printer: p,
		pName:   pName,
		ver:     version,
	}
}

func newProductDownloaderCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:     "downloader <product-name> <version>",
		Short:   "Download file version of product named",
		Long:    "",
		Example: "",
		Args:    withArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.From(cmd.Context())
			p := logging.From(cmd.Context())

			if cfg == nil {
				return errors.New("unable to load configuration")
			}

			pName := args[0]
			ver := args[1]

			this := newDownloaderCmd(
				cfg,
				p,
				pName,
				ver,
			)

			result, err := this.runDownloader(cmd)
			if err != nil {
				return err
			}

			return p.Result(result, func(w io.Writer) error {
				return nil
			})
		},
	}
	flagSet := cmd.Flags()
	flagSet.StringVar(&cmdflags.os, "os", "", "")
	flagSet.StringVar(&cmdflags.arch, "os", "", "")
	flagSet.StringVar(&cmdflags.kind, "kind", "", "")
	flagSet.StringVar(&cmdflags.destDir, "dest-dir", "", "")
	flagSet.StringArrayVar(&cmdflags.files, "file", nil, "")
	flagSet.BoolVar(&cmdflags.all, "all", false, "")
	flagSet.BoolVar(&cmdflags.extract, "extract", false, "")
	flagSet.BoolVar(&cmdflags.noVerify, "no-verify", false, "")
	flagSet.BoolVar(&cmdflags.verifySignature, "verify-signature", false, "")
	return &cmd
}

func (c downloaderCmd) runDownloader(cmd *cobra.Command) (any, error) {

	return nil, nil
}
