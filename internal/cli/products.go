package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/httpclient"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

const productsEndpoint = "products"

const productDescription = `List and search the products of the server (GET <server-url>/products).

Without a criterion, every product is listed. Each criterion becomes a parameter of the
search, and a product must match all of them:

  -Q, --query TEXT        q             text searched in the name, display name and description
  -k, --keyword K         keyword       a keyword of the product (repeat it: all must match)
  -l, --label k=v         label         a label of the product (repeat it: all must match)
      --owner O           owner         a name or an email of the owners
      --license L         license       the license, such as MIT or Apache-2.0
      --deprecated BOOL   deprecated    deprecated products only (true) or none (false)
      --prerelease BOOL   prerelease    products with a prerelease (true) or without (false)
      --updated-since D   updatedSince  updated on or after D (2026-01-31 or RFC 3339)
      --sort KEY          sort          the sort order of the server
      --limit N           limit         products per page
  -c, --cursor C          cursor        the page to read, given by "next" in the previous one

--deprecated and --prerelease filter only when they are given: without them, both kinds are
listed. A search returns one page, and the cursor of the next one is printed on stderr:
--all follows "next" up to the last page.

With -o json, the result is {"products": [...], "next": "<cursor>"}, each product as the
server returns it; next is there only when a page follows.`

type productsFlags struct {
	q            string
	keywords     []string
	labels       []string
	owner        string
	license      string
	deprecated   bool
	prerelease   bool
	updatedSince string
	sort         string
	limit        int
	cursor       string
	all          bool
}

type productsCmd struct {
	cfg *config.Config
	log *slog.Logger
}

type productSummary struct {
	Product     string  `json:"product"`
	DisplayName string  `json:"displayName"`
	Description string  `json:"description"`
	Latest      *string `json:"latest"`
	UpdatedAt   string  `json:"updatedAt"`
}

type productsPage struct {
	Items []json.RawMessage `json:"items"`
	Next  string            `json:"next"`
}

type productsResult struct {
	Products []json.RawMessage `json:"products"`
	Next     string            `json:"next,omitempty"`

	summaries []productSummary
}

func newProductsCommand() *cobra.Command {
	var flags productsFlags
	cmd := cobra.Command{
		Use:   "products",
		Short: "List and search the products",
		Long:  productDescription,
		Example: `  # Every product, one page
  lrctl products

  # Every product, all the pages
  lrctl products --all

  # A text search
  lrctl products --query gitlab

  # The plugins with both keywords and a label
  lrctl products --keyword vcs --keyword plugin --label team=devtools

  # The deprecated products of an owner
  lrctl products --owner devtools@example.com --deprecated

  # Updated since the start of the year, 20 per page, then the next page
  lrctl products --updated-since 2026-01-01 --limit 20
  lrctl products --updated-since 2026-01-01 --limit 20 --cursor <next>

  # The names alone, with jq
  lrctl products --all -o json | jq -r '.products[].product'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("the configuration is not loaded")
			}

			query, err := flags.query(cmd)
			if err != nil {
				return err
			}

			this := newProductsCmd(cfg, p.Logger())
			result, err := this.runProducts(cmd, query, flags.all)
			if err != nil {
				return err
			}
			return this.displayResult(result, p)
		},
	}

	fs := cmd.Flags()
	fs.StringVarP(&flags.q, "query", "Q", "", "Text searched in the name, display name and description")
	fs.StringArrayVarP(&flags.keywords, "keyword", "k", nil, "A keyword of the product (repeatable: all must match)")
	fs.StringArrayVarP(&flags.labels, "label", "l", nil, "A label of the product, as key=value (repeatable: all must match)")
	fs.StringVar(&flags.owner, "owner", "", "A name or an email of the owners")
	fs.StringVar(&flags.license, "license", "", "The license of the product, such as MIT or Apache-2.0")
	fs.BoolVar(&flags.deprecated, "deprecated", false, "Deprecated products only, or none with --deprecated=false")
	fs.BoolVar(&flags.prerelease, "prerelease", false, "Products with a prerelease, or without with --prerelease=false")
	fs.StringVar(&flags.updatedSince, "updated-since", "", "Products updated on or after this date (2026-01-31 or RFC 3339)")
	fs.StringVar(&flags.sort, "sort", "", "The sort order of the server")
	fs.IntVar(&flags.limit, "limit", 0, "Products per page (the server's default when 0)")
	fs.StringVarP(&flags.cursor, "cursor", "c", "", "The page to read, given by \"next\" in the previous one")
	fs.BoolVar(&flags.all, "all", false, "Follow \"next\" up to the last page")

	return &cmd
}

func newProductsCmd(cfg *config.Config, log *slog.Logger) *productsCmd {
	return &productsCmd{
		cfg: cfg,
		log: log.With("cmd", "products"),
	}
}

func (f *productsFlags) query(cmd *cobra.Command) (url.Values, error) {
	q := url.Values{}
	set := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	set("q", f.q)
	for _, k := range f.keywords {
		if k == "" {
			return nil, config.Usage(errors.New("--keyword cannot be empty"))
		}
		q.Add("keyword", k)
	}
	for _, l := range f.labels {
		if k, _, ok := strings.Cut(l, "="); !ok || k == "" {
			return nil, config.Usage(fmt.Errorf("--label %q must be key=value", l))
		}
		q.Add("label", l)
	}
	set("owner", f.owner)
	set("license", f.license)
	if cmd.Flags().Changed("deprecated") {
		q.Set("deprecated", strconv.FormatBool(f.deprecated))
	}
	if cmd.Flags().Changed("prerelease") {
		q.Set("prerelease", strconv.FormatBool(f.prerelease))
	}
	if f.updatedSince != "" {
		if _, err := time.Parse(time.DateOnly, f.updatedSince); err != nil {
			if _, err := time.Parse(time.RFC3339, f.updatedSince); err != nil {
				return nil, config.Usage(fmt.Errorf("--updated-since %q must be a date (2026-01-31) or an RFC 3339 time", f.updatedSince))
			}
		}
		q.Set("updatedSince", f.updatedSince)
	}
	set("sort", f.sort)
	if f.limit < 0 {
		return nil, config.Usage(fmt.Errorf("--limit %d cannot be negative", f.limit))
	}
	if f.limit > 0 {
		q.Set("limit", strconv.Itoa(f.limit))
	}
	set("cursor", f.cursor)
	return q, nil
}

func (c *productsCmd) runProducts(cmd *cobra.Command, query url.Values, all bool) (*productsResult, error) {
	result := &productsResult{Products: []json.RawMessage{}}
	seen := map[string]bool{query.Get("cursor"): true}
	for {
		page, err := c.fetchPage(cmd, query)
		if err != nil {
			return nil, err
		}
		for _, raw := range page.Items {
			var s productSummary
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("fetch the products: a product is not an object: %s", raw)
			}
			result.Products = append(result.Products, raw)
			result.summaries = append(result.summaries, s)
		}
		if !all || page.Next == "" {
			result.Next = page.Next
			return result, nil
		}
		if seen[page.Next] {
			return nil, fmt.Errorf("fetch the products: the server returned the cursor %q twice", page.Next)
		}
		seen[page.Next] = true
		query.Set("cursor", page.Next)
	}
}

func (c *productsCmd) fetchPage(cmd *cobra.Command, query url.Values) (*productsPage, error) {
	ctx := cmd.Context()
	reqURL := c.cfg.Server.BaseUrl + "/" + productsEndpoint
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}
	var params []httpclient.OptionParamItem
	for k, vs := range query {
		for _, v := range vs {
			params = append(params, httpclient.NewRequestOption(k, v))
		}
	}
	start := time.Now()

	resp, err := c.cfg.HttpClient().Get(ctx, productsEndpoint, &httpclient.RequestOptions{QueryParams: params})
	if err != nil {
		var ce *httpclient.ClientError
		if errors.As(err, &ce) && ce.Message != "" {
			err = errors.New(ce.Message)
		}
		c.log.DebugContext(ctx, "request", "method", http.MethodGet, "url", reqURL, "err", err)
		return nil, fmt.Errorf("fetch the products: %w", err)
	}
	defer resp.Body.Close()
	c.log.DebugContext(ctx, "request", "method", http.MethodGet, "url", reqURL,
		"status", resp.StatusCode, "duration", time.Since(start))

	if !httpclient.IsSuccess(resp) {
		d, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		c.log.DebugContext(ctx, "response", "url", reqURL, "body", string(d))
		return nil, fmt.Errorf("fetch the products from %s: %s", reqURL, resp.Status)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch the products from %s: %w", reqURL, err)
	}
	page, err := decodeProductsPage(b)
	if err != nil {
		c.log.DebugContext(ctx, "response", "url", reqURL, "err", err, "body", string(b[:min(len(b), 1<<10)]))
		return nil, fmt.Errorf("fetch the products from %s: the response is not a list of products", reqURL)
	}
	return page, nil
}

func decodeProductsPage(b []byte) (*productsPage, error) {
	var page productsPage
	if trimmed := bytes.TrimSpace(b); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &page.Items); err != nil {
			return nil, err
		}
		return &page, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["items"]; !ok {
		return nil, errors.New("no items")
	}
	if err := json.Unmarshal(b, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *productsCmd) displayResult(result *productsResult, p *logging.Printer) error {
	err := p.Result(result, func(w io.Writer) error {
		tab := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		errs := []error{printText(tab, "PRODUCT\tLATEST\tUPDATED\tDISPLAY NAME")}
		for _, s := range result.summaries {
			latest := "-"
			if s.Latest != nil && *s.Latest != "" {
				latest = *s.Latest
			}
			errs = append(errs, printText(tab, "%s\t%s\t%s\t%s", s.Product, latest, orDash(s.UpdatedAt), orDash(s.DisplayName)))
		}
		errs = append(errs, tab.Flush())
		return errors.Join(errs...)
	})
	if err == nil && result.Next != "" {
		c.log.Info("more products", "next", "--cursor "+result.Next)
	}
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
