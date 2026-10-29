package mcpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/apiclient"
	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/obs"
)

// RunStdio serves MCP on stdin/stdout for local clients (Claude Desktop,
// Claude Code). It drives a remote instance through its REST API:
// CLEADS_URL and CLEADS_API_KEY are required, CLEADS_MCP_MAX_WAIT is
// optional. Diagnostics go to stderr (stdout carries the protocol).
func RunStdio(ctx context.Context, version string, getenv func(string) string, transport mcp.Transport, stderr io.Writer) error {
	key := strings.TrimSpace(getenv("CLEADS_API_KEY"))
	red := obs.NewRedactor(key)
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: obs.ReplaceAttr(red)}))
	client, err := apiclient.New(getenv("CLEADS_URL"), key, version, nil)
	if err != nil {
		return err
	}
	maxWait := 120 * time.Second
	if v := getenv("CLEADS_MCP_MAX_WAIT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("CLEADS_MCP_MAX_WAIT: %q is not a duration", v)
		}
		maxWait = d
	}
	// Fail soft: report an unreachable API on stderr, keep serving so the
	// agent gets a clear tool error instead of a dead server.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if _, err := client.Sources(cctx); err != nil {
		log.Warn("CLEADS API check failed", "url", client.BaseURL(), "err", err)
	} else {
		log.Info("connected", "url", client.BaseURL())
	}
	cancel()
	srv := New(Options{
		Backend:   func(*mcp.RequestExtra) (backend.Backend, error) { return client, nil },
		Version:   version,
		PublicURL: client.BaseURL(),
		MaxWait:   maxWait,
		PollEvery: 2 * time.Second, // each poll is a REST call counted by the per-key rate limit
		Logger:    log,
		Redactor:  red,
	})
	if transport == nil {
		transport = &mcp.StdioTransport{}
	}
	return srv.Run(ctx, transport)
}
