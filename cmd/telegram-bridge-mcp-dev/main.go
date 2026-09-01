package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nextster/telegram-bridge/internal/mcpproxy"
)

const defaultEndpoint = "https://telegram-bridge.fly.dev/mcp"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	endpoint := os.Getenv("TELEGRAM_BRIDGE_MCP_URL")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	server, closeRemote, err := mcpproxy.New(ctx, endpoint, os.Getenv("TELEGRAM_BRIDGE_MCP_TOKEN"))
	if err == nil {
		defer closeRemote()
		err = server.Run(ctx, &mcp.StdioTransport{})
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "telegram-bridge MCP dev adapter:", err)
		os.Exit(1)
	}
}
