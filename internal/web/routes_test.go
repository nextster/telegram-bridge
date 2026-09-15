package web

import (
	"context"
	"github.com/nextster/telegram-bridge/internal/db"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/oauth"
)

func TestRoutesAllowRootAndMCP(t *testing.T) {
	server := &Server{
		cfg:     config.Config{MCPToken: "secret"},
		monitor: &monitor.Service{},
	}
	handler := server.routes()

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("POST /mcp status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestRoutesMountOAuthWithoutStaticToken(t *testing.T) {
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
		cfg:     config.Config{BotToken: "123:token", PublicBaseURL: "https://telegram-bridge.example", OAuthMode: "on"},
		monitor: &monitor.Service{},
		store:   store,
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

func (stubApprover) OAuthConnectionRevoked(context.Context, string, string) error { return nil }

func TestSplitTermGroups(t *testing.T) {
	got := splitTermGroups("USB-C, Type-C\n1000 lm, 1200 lm\r\n\n31.8")
	if len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("groups = %#v", got)
	}
}

func TestNotificationAPIDoesNotExposeMCPWrites(t *testing.T) {
	store, err := db.Open(context.Background(), t.TempDir()+"/notifications.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{MCPToken: "secret", NotificationToken: strings.Repeat("n", 32)}
		if enabled {
			cfg.NotificationChatIDs = []int64{-1001234567890}
		}
		server := &Server{cfg: cfg, monitor: &monitor.Service{}, store: store}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("tools/list status=%d body=%s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "telegram_send_notification") {
			t.Fatalf("incorrect notification capability when enabled=%t", enabled)
		}
		for _, auth := range []string{"Bearer secret", "Bearer " + cfg.NotificationToken} {
			req := httptest.NewRequest(http.MethodPost, "/notifications/v1/messages", strings.NewReader("bad json"))
			req.Header.Set("Authorization", auth)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			server.routes().ServeHTTP(rec, req)
			want := 404
			if enabled {
				want = 401
				if auth == "Bearer "+cfg.NotificationToken {
					want = 400
				}
			}
			if rec.Code != want {
				t.Fatalf("notification API status=%d want=%d", rec.Code, want)
			}
		}

	}
}
