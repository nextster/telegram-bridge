package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/apitoken"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/oauth"
)

func TestRoutesMountOAuth(t *testing.T) {
	store, err := db.Open(context.Background(), t.TempDir()+"/oauth-routes.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oauthServer, err := oauth.New("https://telegram-bridge.example", store, stubApprover{}, oauth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:      config.Config{BotToken: "123:token", PublicBaseURL: "https://telegram-bridge.example", OAuthMode: "on"},
		accounts: &fakeAccounts{},
		store:    store,
	}
	server.SetOAuth(oauthServer)
	handler := server.routes()

	for path, want := range map[string]int{
		"/.well-known/oauth-authorization-server":     http.StatusOK,
		"/.well-known/oauth-protected-resource/mcp":   http.StatusOK,
		"/.well-known/oauth-protected-resource/other": http.StatusNotFound,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want {
			t.Fatalf("GET %s status = %d, want %d", path, response.Code, want)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Header().Get("WWW-Authenticate"), "https://telegram-bridge.example/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("POST /mcp status=%d challenge=%q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
}

type stubApprover struct{}

func (stubApprover) OAuthApprovalLink(context.Context, string) (string, error) {
	return "https://t.me/bridge_test_bot", nil
}

func (stubApprover) OAuthConnectionRevoked(context.Context, int64, string, string) error { return nil }

func (stubApprover) OAuthConnectionCreated(context.Context, int64, string, string) error { return nil }

func TestSplitTermGroups(t *testing.T) {
	got := splitTermGroups("USB-C, Type-C\n1000 lm, 1200 lm\r\n\n31.8")
	if len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("groups = %#v", got)
	}
}

func TestPersonalTokensHaveSeparateScopes(t *testing.T) {
	store, err := db.Open(context.Background(), t.TempDir()+"/tokens.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	mcpToken, mcpHash, _ := apitoken.New(db.APITokenScopeMCP)
	notifyToken, notifyHash, _ := apitoken.New(db.APITokenScopeNotify)
	if _, err := store.CreateAPIToken(ctx, aliceID, db.APITokenScopeMCP, "", mcpHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAPIToken(ctx, aliceID, db.APITokenScopeNotify, "", notifyHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Config{}, accounts: &fakeAccounts{}, store: store}
	handler := server.routes()

	for token, want := range map[string]int{"": 401, "tbm_unknown": 401, notifyToken: 401, mcpToken: 200} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("MCP with %.8q status=%d want=%d body=%s", token, response.Code, want, response.Body.String())
		}
		if want == 200 && strings.Contains(response.Body.String(), "telegram_send_notification") {
			t.Fatal("MCP exposes notification writes")
		}
	}

	for token, want := range map[string]int{"": 401, mcpToken: 401, notifyToken: 400} {
		request := httptest.NewRequest(http.MethodPost, "/notifications/v1/messages", strings.NewReader("bad json"))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("notification API with %.8q status=%d want=%d", token, response.Code, want)
		}
	}
}
