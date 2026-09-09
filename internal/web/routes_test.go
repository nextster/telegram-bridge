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

func TestSplitTermGroups(t *testing.T) {
	got := splitTermGroups("USB-C, Type-C\n1000 lm, 1200 lm\r\n\n31.8")
	if len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("groups = %#v", got)
	}
}

func TestNotificationToolNeedsNoBot(t *testing.T) {
	store, err := db.Open(context.Background(), t.TempDir()+"/notifications.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{MCPToken: "secret"}
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
		if strings.Contains(response.Body.String(), "telegram_send_notification") != enabled {
			t.Fatalf("incorrect notification capability when enabled=%t", enabled)
		}
	}
}
