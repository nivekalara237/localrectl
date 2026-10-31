package cli

import (
	"log/slog"

	"github.com/nivekalara237/localrectl/config"
)

var (
	productIndexEndpoint = "%s/index"
)

type productVersionsCmd struct {
	log    *slog.Logger
	config *config.Config
}

func newProductVersionsCmd(cfg *config.Config, log *slog.Logger) *productVersionsCmd {
	return &productVersionsCmd{
		log:    log.With("versions", "cmd"),
		config: cfg,
	}
}
