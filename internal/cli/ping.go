package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

type pingResult struct {
	URL       string `json:"url"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	TokenSet  bool   `json:"token_set"`
}

// newPingCommand is a command to ping the local-registry server "lrctl ping -o json"
func newPingCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Check the access to the server (GET /healthz)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logging.From(cmd.Context())
			cfg := config.From(cmd.Context())
			if cfg == nil {
				return errors.New("the configuration is not loaded")
			}

			res, err := pingServer(cmd.Context(), cfg, p.Logger())
			if err != nil {
				return err
			}
			return p.Result(res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s: %d in %dms, token set: %v\n",
					res.URL, res.Status, res.LatencyMs, res.TokenSet)
				return err
			})
		},
	}
}

func pingServer(ctx context.Context, cfg *config.Config, log *slog.Logger) (*pingResult, error) {
	log = log.With("cmd", "ping")
	client := http.Client{Timeout: time.Duration(cfg.Timeout) * time.Second}
	url := cfg.Server.BaseUrl + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	response, err := client.Do(req)
	if err != nil {
		log.DebugContext(ctx, "request", "method", req.Method, "url", url, "err", err)
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<10))
	latency := time.Since(start)
	if err != nil {
		log.DebugContext(ctx, "request", "method", req.Method, "url", url, "err", err)
		return nil, fmt.Errorf("%s: %w", url, err)
	}

	log.DebugContext(ctx, "request", "method", req.Method, "url", url,
		"status", response.StatusCode, "duration", latency, "body", string(body))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", url, response.Status)
	}
	return &pingResult{
		URL:       url,
		Status:    response.StatusCode,
		LatencyMs: latency.Milliseconds(),
		TokenSet:  cfg.Server.UploadToken != "",
	}, nil
}
