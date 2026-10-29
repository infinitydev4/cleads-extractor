// Command mcp is a stdio MCP server for local clients (Claude Desktop,
// Claude Code). It exposes the same tools as the /mcp endpoint but drives a
// remote extractor through its REST API, without database access:
//
//	CLEADS_URL=https://api.example.com CLEADS_API_KEY=... cleads-mcp
//
// The server binary offers the same mode as "cleads-extractor mcp".
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cleads/business-extractor/internal/mcpserver"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := mcpserver.RunStdio(ctx, version, os.Getenv, nil, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
