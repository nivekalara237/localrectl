package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/httpclient"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	productIndexEndpoint = "%s/index"
)

type indexResponse struct {
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	License     string `json:"license,omitempty"`
	Owners      []struct {
		Name  string `json:"name"`
		Email string `json:"email,omitempty"`
	} `json:"owners,omitempty"`
	Links []struct {
		Name string `json:"name,omitempty"`
		Url  string `json:"url"`
	} `json:"links,omitempty"`
	Keywords  []string       `json:"keywords,omitempty"`
	Labels    map[string]any `json:"labels,omitempty"`
	Install   string         `json:"install"`
	Latest    string         `json:"latest"`
	UpdatedAt string         `json:"updatedAt,omitempty"`
	Versions  []vItem        `json:"versions"`
}

type vItem struct {
	Version         string `json:"version"`
	Tag             string `json:"tag"`
	PublishedAt     string `json:"publishedAt"`
	SecurityUpdates bool   `json:"securityUpdates"`
	Deprecated      bool   `json:"deprecated"`
	Prerelease      bool   `json:"prerelease"`
}

type productVersionsCmd struct {
	log    *slog.Logger
	config *config.Config
}

type versionsResult struct {
	index *indexResponse
	err   error
}

func newProductVersionsCmd(cfg *config.Config, log *slog.Logger) *productVersionsCmd {
	return &productVersionsCmd{
		log:    log.With("cmd", "versions"),
		config: cfg,
	}
}

func newVersionsCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:   "versions <product-name>",
		Short: "Get a list of versions of a product",
		Long: `List the versions of a product, as published in its index on the server
(GET <server-url>/<product-name>/index), in the order of the index.

The table gives the version, its tag, whether it is a security update and its publication
date. The version is marked (Latest) for the latest version, (Deprecated) for a deprecated
one and (Prerelease) for a prerelease. The prereleases are hidden unless --prerelease (or
--pre) is given.

With -o json, the same versions are printed as an array of objects with the keys version,
tag, publishedAt, securityUpdates, deprecated and prerelease: an empty array when the
product has none. A product that does not exist fails with "not found".`,
		Example: `  # The versions of a product
  lrctl versions bb-vcs-plugin

  # With the prereleases
  lrctl versions bb-vcs-plugin --prerelease

  # The version numbers alone, with jq
  lrctl versions bb-vcs-plugin -o json | jq -r '.[].version'

  # The release.json of one of them
  lrctl release bb-vcs-plugin v2.0.0`,
		Args:    productNameArg,
		Aliases: []string{"product-versions"},
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("the configuration is not loaded")
			}

			this := newProductVersionsCmd(cfg, p.Logger())
			productName := args[0]
			result, err := this.runProductVersionsSearch(cmd, productName)
			if err != nil {
				return err
			}
			pre, _ := cmd.Flags().GetBool("prerelease")
			return this.displayResult(result, p, pre)
		},
	}
	cmd.Flags().Bool("prerelease", false, "fetch the staging versions")

	cmd.Flags().SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "pre" {
			name = "prerelease"
		}
		return pflag.NormalizedName(name)
	})

	return &cmd
}

func productNameArg(cmd *cobra.Command, args []string) error {
	if err := withArgs(1)(cmd, args); err != nil {
		return err
	}
	return pathSegmentArg("product name", args[0])
}

func pathSegmentArg(what, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return config.Usage(fmt.Errorf("invalid %s %q: it cannot be empty, . or .., nor hold / or \\", what, value))
	}
	return nil
}

func (v *productVersionsCmd) runProductVersionsSearch(cmd *cobra.Command, productName string) (*versionsResult, error) {
	ctx := cmd.Context()
	uri := fmt.Sprintf(productIndexEndpoint, productName)
	reqURL := v.config.Server.BaseUrl + "/" + uri
	start := time.Now()
	client := v.config.HttpClient()

	resp, err := client.Get(ctx, uri, nil)
	if err != nil {
		var ce *httpclient.ClientError
		if errors.As(err, &ce) && ce.Message != "" {
			err = errors.New(ce.Message)
		}
		v.log.DebugContext(ctx, "request", "method", http.MethodGet, "url", reqURL, "err", err)
		return nil, fmt.Errorf("fetch the product versions: %w", err)
	}
	defer resp.Body.Close()
	v.log.DebugContext(ctx, "request", "method", http.MethodGet, "url", reqURL,
		"status", resp.StatusCode, "duration", time.Since(start))

	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New(productName + " not found")
	}

	if !httpclient.IsSuccess(resp) {
		d, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		v.log.DebugContext(ctx, "response", "url", reqURL, "body", string(d))
		return nil, fmt.Errorf("fetch the product versions from %s: %s", reqURL, resp.Status)
	}

	body := httpclient.ToPojo[indexResponse](resp)
	if body == nil {
		v.log.DebugContext(ctx, "response", "url", reqURL, "err", "not an index")
		return nil, fmt.Errorf("fetch the product versions from %s: the response is not an index", reqURL)
	}
	return &versionsResult{index: body}, nil
}

func (v *productVersionsCmd) displayResult(result *versionsResult, p *logging.Printer, pre bool) error {
	if result.index == nil {
		return errors.New("uninspected during handling versions fetched")
	}
	type versionItem struct {
		Version        string `json:"Version"`
		Tag            string `json:"Tag"`
		SecurityUpdate string `json:"SecurityUpdate"`
		PublishedAt    string `json:"PublishedAt"`
	}

	listed := []vItem{}
	var versions []versionItem

	for _, ver := range result.index.Versions {
		if ver.Prerelease && !pre {
			continue
		}
		listed = append(listed, ver)
		var suffixes []string
		if ver.Prerelease {
			suffixes = append(suffixes, "Prerelease")
		}
		if ver.Version == result.index.Latest {
			suffixes = append(suffixes, "Latest")
		}
		if ver.Deprecated {
			suffixes = append(suffixes, "Deprecated")
		}
		suffix := ""
		if len(suffixes) > 0 {
			suffix = " (" + strings.Join(suffixes, ", ") + ")"
		}
		tag := ver.Tag
		if tag == "" {
			tag = "-"
		}
		sec := "-"
		if ver.SecurityUpdates {
			sec = "True"
		}
		versions = append(versions, versionItem{
			Version:        ver.Version + suffix,
			Tag:            tag,
			SecurityUpdate: sec,
			PublishedAt:    ver.PublishedAt,
		})
	}

	displayError := p.Result(listed, func(w io.Writer) error {
		tab := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		var errs []error
		errs = append(errs, printText(tab, "VERSION\tTAG\tSECURITY UPDATE\tPUBLISHED"))
		for _, ver := range versions {
			errs = append(errs, printText(tab, "%s\t%s\t%s\t%s", ver.Version, ver.Tag, ver.SecurityUpdate, ver.PublishedAt))
		}
		errs = append(errs, tab.Flush())
		return errors.Join(errs...)
	})

	return displayError
}
