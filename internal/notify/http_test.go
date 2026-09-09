package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nextster/telegram-bridge/internal/db"
)

func TestNotificationAPIAuthAndInputBoundaries(t *testing.T) {
	service, sender, _ := setupNotifications(t)
	token := strings.Repeat("n", 32)
	handler := NewHTTPHandler(service, token)
	valid := `{"chat":"channel:1234567890","event_id":"release:1","text":"Release test"}`
	for _, tc := range []struct {
		name, method, auth, contentType, body string
		status                                int
	}{
		{"no auth", "POST", "", "application/json", valid, 401},
		{"MCP credential", "POST", "Bearer read-only-mcp-token", "application/json", valid, 401},
		{"worker credential", "POST", "Bearer worker-token", "application/json", valid, 401},
		{"method", "GET", "Bearer " + token, "application/json", valid, 405},
		{"content type", "POST", "Bearer " + token, "text/plain", valid, 415},
		{"unknown field", "POST", "Bearer " + token, "application/json", `{"chat":"channel:1234567890","event_id":"release:1","text":"hello","parse_mode":"HTML"}`, 400},
		{"trailing JSON", "POST", "Bearer " + token, "application/json", valid + ` {}`, 400},
		{"oversize", "POST", "Bearer " + token, "application/json", strings.Repeat(" ", 32769) + valid, 400},
		{"bad JSON", "POST", "Bearer " + token, "application/json", `{`, 400},
		{"missing fields", "POST", "Bearer " + token, "application/json", `{}`, 422},
		{"other group", "POST", "Bearer " + token, "application/json", strings.Replace(valid, "1234567890", "1", 1), 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/notifications/v1/messages", strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), token) {
				t.Fatal("response leaked token")
			}
			if sender.calls.Load() != 0 {
				t.Fatal("rejected request sent a message")
			}
		})
	}
}

func TestNotificationAPIReturnsDurableReceipt(t *testing.T) {
	service, sender, _ := setupNotifications(t)
	handler := NewHTTPHandler(service, strings.Repeat("n", 32))
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/notifications/v1/messages", strings.NewReader(`{"chat":"channel:1234567890","event_id":"release:1","text":"Release test"}`))
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("n", 32))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var receipt db.NotificationReceipt
		if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil || receipt.Status != "sent" || receipt.MessageID != 42 {
			t.Fatalf("invalid receipt: %s", rec.Body.String())
		}
	}
	if sender.calls.Load() != 1 {
		t.Fatal("duplicate API call sent twice")
	}
}

func TestNotificationAPIRedactsUncertainSend(t *testing.T) {
	service, sender, _ := setupNotifications(t)
	sender.fail = true
	handler := NewHTTPHandler(service, strings.Repeat("n", 32))
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/notifications/v1/messages", strings.NewReader(`{"chat":"channel:1234567890","event_id":"release:1","text":"Release test"}`))
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("n", 32))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 422 || strings.Contains(rec.Body.String(), "private token") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if sender.calls.Load() != 1 {
		t.Fatal("uncertain API call resent")
	}
}
