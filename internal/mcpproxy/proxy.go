package mcpproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// New connects to the production HTTP MCP and exposes the same tools through a
// local server. It never opens the Telegram session itself.
func New(ctx context.Context, endpoint, token string) (*mcp.Server, func() error, error) {
	endpoint = strings.TrimSpace(endpoint)
	token = strings.TrimSpace(token)
	if endpoint == "" {
		return nil, nil, errors.New("MCP endpoint is required")
	}
	if token == "" {
		return nil, nil, errors.New("TELEGRAM_BRIDGE_MCP_TOKEN is required")
	}
	httpClient := &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "telegram-bridge-dev-adapter", Version: "1"}, nil)
	remote, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to Telegram Bridge MCP: %w", err)
	}
	closeRemote := remote.Close
	tools, err := listAllTools(ctx, remote)
	if err != nil {
		_ = closeRemote()
		return nil, nil, err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "telegram-bridge-dev", Version: "1"}, nil)
	for _, tool := range tools {
		tool := tool
		server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return remote.CallTool(ctx, &mcp.CallToolParams{
				Meta:      request.Params.Meta,
				Name:      tool.Name,
				Arguments: request.Params.Arguments,
			})
		})
	}
	return server, closeRemote, nil
}

func listAllTools(ctx context.Context, remote *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	cursor := ""
	for {
		result, err := remote.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list Telegram Bridge MCP tools: %w", err)
		}
		for _, tool := range result.Tools {
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				return nil, fmt.Errorf("refusing non-read-only MCP tool %q", tool.Name)
			}
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
}
