package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
)

const testOwnerID = 111

type telegramCall struct {
	Method string
	Body   map[string]any
}

type fakeTelegram struct {
	mu    sync.Mutex
	calls []telegramCall
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, telegramCall{Method: method, Body: body})
	f.mu.Unlock()
	result := any(true)
	switch method {
	case "sendMessage", "editMessageText", "editMessageReplyMarkup":
		result = map[string]any{"message_id": 5, "date": 0, "chat": map[string]any{"id": body["chat_id"], "type": "private"}}
	case "getMe":
		result = map[string]any{"id": 1, "is_bot": true, "first_name": "Bridge", "username": "bridge_test_bot"}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeTelegram) take() []telegramCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func newOAuthTestService(t *testing.T) (*Service, *db.Store, *fakeTelegram) {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/bot-oauth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	api := &fakeTelegram{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	bot, err := telego.NewBot("123456:"+strings.Repeat("a", 35),
		telego.WithAPIServer(server.URL),
		telego.WithAPICaller(telegoapi.HTTPCaller{Client: server.Client()}),
		telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{bot: bot, store: store, cfg: config.Config{}, accounts: &fakeAccounts{connected: map[int64]bool{testOwnerID: true}}}
	return service, store, api
}

func createPendingOAuthRequest(t *testing.T, store *db.Store, id, clientName string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	clientID := "tbc_" + id
	if err := store.CreateOAuthClient(ctx, db.OAuthClient{ClientID: clientID, Name: clientName, RedirectURIs: []string{"http://127.0.0.1:1/cb"}, AuthMethod: "none"}, 10, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateOAuthRequest(ctx, db.OAuthRequest{
		ID: id, BrowserHash: "hash", ClientID: clientID, RedirectURI: "http://127.0.0.1:1/cb",
		CodeChallenge: strings.Repeat("c", 43), Resource: "https://example/mcp",
		ClientIP: "203.0.113.7", UserAgent: "Mozilla/5.0 <script>", ExpiresAt: now.Add(time.Minute),
	}, 10, now); err != nil {
		t.Fatal(err)
	}
}

func startMessage(from, chat int64, text string) telego.Update {
	return telego.Update{Message: &telego.Message{
		MessageID: 9,
		From:      &telego.User{ID: from},
		Chat:      telego.Chat{ID: chat, Type: "private"},
		Text:      text,
	}}
}

func oauthCallback(from, chat int64, data string) telego.Update {
	return telego.Update{CallbackQuery: &telego.CallbackQuery{
		ID:      "query",
		From:    telego.User{ID: from},
		Message: &telego.Message{MessageID: 5, Chat: telego.Chat{ID: chat, Type: "private"}},
		Data:    data,
	}}
}

func TestOAuthBotLinkAndConnectedAccounts(t *testing.T) {
	service, _, _ := newOAuthTestService(t)
	ctx := context.Background()
	if link, err := service.OAuthBotLink(ctx); err != nil || link != "https://t.me/bridge_test_bot" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	for userID, want := range map[int64]bool{testOwnerID: true, 222: false, 0: false} {
		if connected, err := service.OAuthAccountConnected(ctx, userID); err != nil || connected != want {
			t.Fatalf("OAuthAccountConnected(%d) = %t, %v", userID, connected, err)
		}
	}
}

func TestOAuthConnectionAlertOffersRevoke(t *testing.T) {
	service, _, api := newOAuthTestService(t)
	if err := service.OAuthConnectionCreated(context.Background(), testOwnerID, "grant-1", "Claude /stop @someone", "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if len(calls) != 1 || calls[0].Body["chat_id"] != float64(testOwnerID) || calls[0].Body["parse_mode"] != "HTML" {
		t.Fatalf("alert calls = %#v", calls)
	}
	text := calls[0].Body["text"].(string)
	if !strings.Contains(text, "<code>Claude /stop @someone</code>") || !strings.Contains(text, "<code>203.0.113.7</code>") {
		t.Fatalf("alert text = %q", text)
	}
	markup, _ := json.Marshal(calls[0].Body["reply_markup"])
	if !strings.Contains(string(markup), `"oauthrevoke:grant-1"`) {
		t.Fatalf("alert markup = %s", markup)
	}
}
