package cli

import "github.com/spf13/cobra"

type productResult struct {
	URL       string `json:"url"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Items     any    `json:"items"`
}

const productDescription = `
lrctl products <command> [args]
`

func newProductsCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:   "products",
		Short: "Get details about a specific product",
		Long:  productDescription,
		Example: `lrctl products validate /path/to/registry.yaml \
						--product-name=gitlab-plugin \
						--Artifacts=/path/to/Artifacts.json
		`,
	}

	return &cmd
}
