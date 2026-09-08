package mcpproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProxyPreservesToolsAndForwardsCalls(t *testing.T) {
	for _, name := range []string{"telegram_test", "telegram_send_notification"} {
		t.Run(name, func(t *testing.T) {
			backend := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
			mcp.AddTool(backend, &mcp.Tool{
				Name:        name,
				Description: "read-only test",
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: name == "telegram_test", IdempotentHint: name == "telegram_send_notification"},
			}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct {
				OK bool `json:"ok"`
			}, error) {
				return nil, struct {
					OK bool `json:"ok"`
				}{OK: true}, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, &mcp.StreamableHTTPOptions{Stateless: true})
			httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer test-token" {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				handler.ServeHTTP(writer, request)
			}))
			defer httpServer.Close()

			ctx := context.Background()
			proxy, closeRemote, err := New(ctx, httpServer.URL, "test-token")
			if err != nil {
				t.Fatal(err)
			}
			defer closeRemote()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverSession, err := proxy.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer serverSession.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			clientSession, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer clientSession.Close()
			tools, err := clientSession.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(tools.Tools) != 1 || tools.Tools[0].Name != name || tools.Tools[0].Annotations == nil || tools.Tools[0].Annotations.ReadOnlyHint != (name == "telegram_test") {
				t.Fatalf("unexpected proxied tools: %#v", tools.Tools)
			}
			result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			structured, ok := result.StructuredContent.(map[string]any)
			if !ok || structured["ok"] != true {
				t.Fatalf("unexpected proxied result: %#v", result.StructuredContent)
			}
		})
	}
}

func TestProxyRequiresToken(t *testing.T) {
	if _, _, err := New(context.Background(), "https://example.invalid/mcp", ""); err == nil {
		t.Fatal("New accepted an empty token")
	}
}

func TestProxyRejectsNonReadOnlyTool(t *testing.T) {
	backend := mcp.NewServer(&mcp.Implementation{Name: "backend", Version: "1"}, nil)
	mcp.AddTool(backend, &mcp.Tool{Name: "telegram_write"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	if _, _, err := New(context.Background(), httpServer.URL, "test-token"); err == nil {
		t.Fatal("New accepted a non-read-only remote tool")
	}
}
