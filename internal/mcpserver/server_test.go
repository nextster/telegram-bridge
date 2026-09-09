package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/notify"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestServerRejectsMissingToken(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	New(nil, "secret", nil).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("WWW-Authenticate header is missing")
	}
}

func TestMCPHandshakeAndToolsList(t *testing.T) {
	httpServer := httptest.NewServer(New(nil, "secret", nil))
	defer httpServer.Close()

	baseTransport := http.DefaultTransport
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.Header.Set("Authorization", "Bearer secret")
		return baseTransport.RoundTrip(clone)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "telegram-bridge-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(result.Tools) != 3 {
		t.Fatalf("tool count = %d, want 3", len(result.Tools))
	}
	foundHistory := false
	for _, tool := range result.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %q is not marked read-only", tool.Name)
		}
		if tool.Name != "telegram_get_history" {
			continue
		}
		foundHistory = true
		rawSchema, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("marshal history output schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(rawSchema, &schema); err != nil {
			t.Fatalf("decode history output schema: %v", err)
		}
		messageProperties := schema.Properties["messages"].Items.Properties
		for _, name := range []string{"reply_to_id", "topic_id", "media_kind", "edited_at"} {
			if _, ok := messageProperties[name]; !ok {
				t.Errorf("history output schema is missing message property %q: %s", name, rawSchema)
			}
		}
	}
	if !foundHistory {
		t.Fatal("telegram_get_history tool is missing")
	}
}

func TestParseDate(t *testing.T) {
	for _, value := range []string{"", "2026-07-18", "2026-07-18T12:30:00Z"} {
		if _, err := parseDate(value); err != nil {
			t.Fatalf("parseDate(%q): %v", value, err)
		}
	}
	if _, err := parseDate("18 July"); err == nil {
		t.Fatal("parseDate accepted invalid input")
	}
}

type notificationSender struct{ sends atomic.Int32 }

func (*notificationSender) CheckNotificationChat(context.Context, int64) error { return nil }
func (s *notificationSender) SendNotification(context.Context, int64, string, string) (int, error) {
	s.sends.Add(1)
	return 42, nil
}

func TestNotificationMCPContract(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/notifications.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sender := &notificationSender{}
	httpServer := httptest.NewServer(New(nil, "secret", notify.NewNotifications(store, sender, []int64{-1001234567890})))
	defer httpServer.Close()
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.Header = r.Header.Clone()
		clone.Header.Set("Authorization", "Bearer secret")
		return http.DefaultTransport.RoundTrip(clone)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "telegram_send_notification" {
			found = true
			if tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatal("wrong write annotations")
			}
		}
	}
	if !found {
		t.Fatal("notification tool missing")
	}
	args := map[string]any{"chat": "channel:1234567890", "event_id": "release:1", "text": "Release test"}
	for range 2 {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "telegram_send_notification", Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		encoded, _ := json.Marshal(result.StructuredContent)
		var receipt db.NotificationReceipt
		if err := json.Unmarshal(encoded, &receipt); err != nil || receipt.MessageID != 42 || receipt.Status != "sent" {
			t.Fatalf("invalid receipt: %s", encoded)
		}
	}
	for _, invalid := range []map[string]any{
		{"chat": "channel:1", "event_id": "release:2", "text": "not allowed"},
		{"chat": "channel:1234567890", "event_id": "release:2", "text": "extra field", "parse_mode": "HTML"},
		{"chat": "channel:1234567890", "event_id": "release:2"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "telegram_send_notification", Arguments: invalid})
		if err == nil && !result.IsError {
			t.Fatal("accepted invalid tool input")
		}
	}
	if sender.sends.Load() != 1 {
		t.Fatal("duplicate or invalid request sent")
	}
	for _, auth := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		New(nil, "secret", notify.NewNotifications(store, sender, []int64{-1001234567890})).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatal("accepted unauthenticated write endpoint")
		}
	}
}
