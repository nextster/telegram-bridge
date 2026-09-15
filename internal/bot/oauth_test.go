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
		CodeChallenge: strings.Repeat("c", 43), Resource: "https://example/mcp", ApprovalCode: "42",
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

func TestOAuthApprovalLinkUsesBotDeepLink(t *testing.T) {
	service, _, _ := newOAuthTestService(t)
	link, err := service.OAuthApprovalLink(context.Background(), "req_1-A")
	if err != nil || link != "https://t.me/bridge_test_bot?start=oauth_req_1-A" {
		t.Fatalf("link=%q err=%v", link, err)
	}
}

func TestOAuthStartShowsPromptOnlyToAccountOwner(t *testing.T) {
	service, store, api := newOAuthTestService(t)
	ctx := context.Background()
	createPendingOAuthRequest(t, store, "req1", "Claude /stop @someone")

	if err := service.handleUpdate(ctx, startMessage(222, 222, "/start oauth_req1")); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if len(calls) != 1 || strings.Contains(calls[0].Body["text"].(string), "42") || calls[0].Body["reply_markup"] != nil {
		t.Fatalf("a user without a connected account received a prompt: %#v", calls)
	}
	if subscribed, _ := store.IsSubscribed(ctx, 222); subscribed {
		t.Fatal("OAuth deep link subscribed a user")
	}

	if err := service.handleUpdate(ctx, startMessage(testOwnerID, testOwnerID, "/start oauth_req1")); err != nil {
		t.Fatal(err)
	}
	calls = api.take()
	if len(calls) != 1 || calls[0].Method != "sendMessage" || calls[0].Body["parse_mode"] != "HTML" {
		t.Fatalf("owner prompt calls = %#v", calls)
	}
	text := calls[0].Body["text"].(string)
	for _, want := range []string{"<code>Claude /stop @someone</code>", "<code>203.0.113.7</code>", "<code>Mozilla/5.0 &lt;script&gt;</code>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt %q is missing %q", text, want)
		}
	}
	markup, _ := json.Marshal(calls[0].Body["reply_markup"])
	if !strings.Contains(string(markup), `"oauth:req1:42"`) || !strings.Contains(string(markup), `"oauth:req1:deny"`) || strings.Count(string(markup), `"oauth:req1:`) != 5 {
		t.Fatalf("markup = %s", markup)
	}
}

func TestOAuthCallbackRequiresOwnerAndMatchingNumber(t *testing.T) {
	service, store, api := newOAuthTestService(t)
	ctx := context.Background()

	createPendingOAuthRequest(t, store, "stranger", "Codex")
	if err := service.handleUpdate(ctx, oauthCallback(222, 222, "oauth:stranger:42")); err != nil {
		t.Fatal(err)
	}
	if request, _, _ := store.GetOAuthRequest(ctx, "stranger"); request.Status != db.OAuthRequestPending {
		t.Fatalf("a user without a connected account changed status to %s", request.Status)
	}
	if err := service.handleUpdate(ctx, oauthCallback(testOwnerID, 333, "oauth:stranger:42")); err != nil {
		t.Fatal(err)
	}
	if request, _, _ := store.GetOAuthRequest(ctx, "stranger"); request.Status != db.OAuthRequestPending {
		t.Fatalf("a press outside the private chat changed status to %s", request.Status)
	}

	createPendingOAuthRequest(t, store, "wrong", "Codex")
	if err := service.handleUpdate(ctx, oauthCallback(testOwnerID, testOwnerID, "oauth:wrong:17")); err != nil {
		t.Fatal(err)
	}
	if request, _, _ := store.GetOAuthRequest(ctx, "wrong"); request.Status != db.OAuthRequestDenied {
		t.Fatalf("wrong number status = %s", request.Status)
	}

	createPendingOAuthRequest(t, store, "right", "Codex")
	api.take()
	if err := service.handleUpdate(ctx, oauthCallback(testOwnerID, testOwnerID, "oauth:right:42")); err != nil {
		t.Fatal(err)
	}
	request, _, _ := store.GetOAuthRequest(ctx, "right")
	if request.Status != db.OAuthRequestApproved || request.DecidedBy != testOwnerID {
		t.Fatalf("correct number request = %+v", request)
	}
	calls := api.take()
	if len(calls) != 2 || calls[0].Method != "editMessageText" || !strings.Contains(calls[0].Body["text"].(string), "Доступ разрешён") {
		t.Fatalf("approval calls = %#v", calls)
	}

	if err := service.handleUpdate(ctx, oauthCallback(testOwnerID, testOwnerID, "oauth:right:deny")); err != nil {
		t.Fatal(err)
	}
	if request, _, _ := store.GetOAuthRequest(ctx, "right"); request.Status != db.OAuthRequestApproved {
		t.Fatalf("second press changed a decided request to %s", request.Status)
	}
}

func TestNoOAuthApprovalWithoutConnectedAccount(t *testing.T) {
	service, store, _ := newOAuthTestService(t)
	service.accounts = &fakeAccounts{connected: map[int64]bool{}}
	ctx := context.Background()
	createPendingOAuthRequest(t, store, "orphan", "Codex")
	if err := service.handleUpdate(ctx, oauthCallback(testOwnerID, testOwnerID, "oauth:orphan:42")); err != nil {
		t.Fatal(err)
	}
	if request, _, _ := store.GetOAuthRequest(ctx, "orphan"); request.Status != db.OAuthRequestPending {
		t.Fatalf("approval without a logged-in owner changed status to %s", request.Status)
	}
}
