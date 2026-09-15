package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nextster/telegram-bridge/internal/monitor"
)

const testUser = int64(1)

func testVerifier(tokens map[string]int64) TokenVerifier {
	return func(_ context.Context, token string) (int64, time.Time, error) {
		userID, ok := tokens[token]
		if !ok {
			return 0, time.Time{}, auth.ErrInvalidToken
		}
		return userID, time.Now().Add(time.Hour), nil
	}
}

func accountsFor(accounts map[int64]Monitor) AccountResolver {
	return func(userID int64) (Monitor, error) {
		account, ok := accounts[userID]
		if !ok {
			return nil, errors.New("not connected")
		}
		return account, nil
	}
}

func testRequest(userID int64) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: strconv.FormatInt(userID, 10), Expiration: time.Now().Add(time.Hour)}}}
}

type namedDialogs struct {
	historyReader
	name string
}

func (d *namedDialogs) ListDialogs(context.Context, string, int) ([]monitor.TelegramDialog, error) {
	return []monitor.TelegramDialog{{Peer: monitor.TelegramPeer{Key: d.name}}}, nil
}

func TestToolsUseOnlyTheAuthenticatedUsersAccount(t *testing.T) {
	server := httptest.NewServer(New(Options{
		VerifyToken: testVerifier(map[string]int64{"alice-token": 1, "bob-token": 2, "carol-token": 3}),
		Accounts:    accountsFor(map[int64]Monitor{1: &namedDialogs{name: "alice"}, 2: &namedDialogs{name: "bob"}}),
	}))
	defer server.Close()

	call := func(token string) (*mcp.CallToolResult, error) {
		httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			clone.Header = r.Header.Clone()
			clone.Header.Set("Authorization", "Bearer "+token)
			return http.DefaultTransport.RoundTrip(clone)
		})}
		client := mcp.NewClient(&mcp.Implementation{Name: "isolation-test", Version: "1"}, nil)
		session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
		if err != nil {
			return nil, err
		}
		defer session.Close()
		return session.CallTool(context.Background(), &mcp.CallToolParams{Name: "telegram_list_dialogs", Arguments: map[string]any{}})
	}

	for token, want := range map[string]string{"alice-token": "alice", "bob-token": "bob"} {
		result, err := call(token)
		if err != nil || result.IsError {
			t.Fatalf("%s call err=%v result=%#v", token, err, result)
		}
		dialogs := result.StructuredContent.(map[string]any)["dialogs"].([]any)
		if len(dialogs) != 1 || dialogs[0].(map[string]any)["peer"].(map[string]any)["key"] != want {
			t.Fatalf("%s read dialogs %#v, want only %s", token, dialogs, want)
		}
	}
	if result, err := call("carol-token"); err != nil || !result.IsError {
		t.Fatalf("a user without a connected account read someone's dialogs: err=%v result=%#v", err, result)
	}
	if _, err := call("forged-token"); err == nil {
		t.Fatal("unknown token connected to MCP")
	}
	if _, err := principal(&mcp.CallToolRequest{}); err == nil {
		t.Fatal("request without token info produced a principal")
	}
}
