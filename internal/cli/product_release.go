package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/httpclient"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

type productReleaseCmd struct {
	log    *slog.Logger
	config *config.Config
}

type productReleaseResult struct {
	raw json.RawMessage
}

const (
	productReleaseEndpoint = "%s/releases/%s"
)

func newProductReleaseCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:   "release <product-name> <version>",
		Short: "Display the release.json of a version of a product",
		Long: `Display the release.json of a version of a product, as published on the server
(GET <server-url>/<product-name>/releases/<version>).

<version> is a version listed by 'lrctl versions <product-name>' (add --prerelease for the
prereleases), or latest for the latest version of the product.

The document is printed as the server returns it, indented, in text as with -o json: pipe
-o json to jq to read a field. A product or a version that does not exist fails with
"not found".`,
		Example: `  # The release.json of a version
  lrctl release gitlab-vcs-plugin v1.6.23

  # The latest version
  lrctl release gitlab-vcs-plugin latest

  # One field of the latest version, with jq
  lrctl release gitlab-vcs-plugin latest -o json | jq -r .version

  # On another server
  lrctl release gitlab-vcs-plugin v1.6.23 --server-url https://registry.example.com`,
		Args:    releaseArgs,
		Aliases: []string{"product-release"},
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())

			if cfg == nil {
				return errors.New("the configuration is not loaded")
			}

			this := newProductReleaseCmd(cfg, p.Logger())
			productName := args[0]
			version := args[1]

			result, err := this.runProductRelease(cmd, productName, version)
			if err != nil {
				return err
			}
			return this.DisplayResult(result, p)
		},
	}

	return &cmd
}

func releaseArgs(cmd *cobra.Command, args []string) error {
	if err := withArgs(2)(cmd, args); err != nil {
		return err
	}
	if err := pathSegmentArg("product name", args[0]); err != nil {
		return err
	}
	return pathSegmentArg("version", args[1])
}

func (r *productReleaseCmd) runProductRelease(cmd *cobra.Command, productName, version string) (*productReleaseResult, error) {
	ctx := cmd.Context()
	uri := fmt.Sprintf(productReleaseEndpoint, productName, version)
	reqURL := r.config.Server.BaseUrl + "/" + uri
	start := time.Now()
	client := r.config.HttpClient()

	resp, err := client.Get(ctx, uri, nil)
	if err != nil {
		var ce *httpclient.ClientError
		if errors.As(err, &ce) && ce.Message != "" {
			err = errors.New(ce.Message)
		}
		r.log.DebugContext(ctx, "request", "method", http.MethodGet, "url", reqURL, "err", err)
		return nil, fmt.Errorf("fetch the product release %s %s: %w", productName, version, err)
	}
	defer resp.Body.Close()
	r.log.DebugContext(ctx, "request", "method", http.MethodGet, "url", reqURL,
		"status", resp.StatusCode, "duration", time.Since(start))

	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New(productName + " or " + version + " not found")
	}

	if !httpclient.IsSuccess(resp) {
		d, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		r.log.DebugContext(ctx, "response", "url", reqURL, "body", string(d))
		return nil, fmt.Errorf("fetch the product release from %s: %s", reqURL, resp.Status)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch the product release from %s: %w", reqURL, err)
	}

	if !json.Valid(b) {
		r.log.DebugContext(ctx, "response", "url", reqURL, "err", "not JSON", "body", string(b[:min(len(b), 1<<10)]))
		return nil, fmt.Errorf("fetch the product release from %s: the response is not JSON", reqURL)
	}

	return &productReleaseResult{
		raw: json.RawMessage(b),
	}, nil
}

func (r *productReleaseCmd) DisplayResult(result *productReleaseResult, p *logging.Printer) error {
	return p.Result(result.raw, func(w io.Writer) error {
		var o bytes.Buffer
		if err := json.Indent(&o, result.raw, "", "  "); err != nil {
			return err
		}
		return printText(w, "%s", o.String())
	})
}

func newProductReleaseCmd(cfg *config.Config, logger *slog.Logger) *productReleaseCmd {
	return &productReleaseCmd{
		log:    logger.With("cmd", "release"),
		config: cfg,
	}
}
