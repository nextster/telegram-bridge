package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/apitoken"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/notify"
)

const (
	aliceID = int64(42)
	bobID   = int64(99)
)

type fakeAccounts struct {
	checked []int64
}

func (*fakeAccounts) Account(int64) (*monitor.Service, error) { return nil, monitor.ErrNotConnected }

func (*fakeAccounts) Status(int64) monitor.Status { return monitor.Status{Configured: true} }

func (*fakeAccounts) Login(context.Context, int64, monitor.LoginOptions) (monitor.LoginResult, error) {
	return monitor.LoginResult{}, monitor.ErrNotConnected
}

func (f *fakeAccounts) NotificationSender(_ context.Context, accountID int64) (notify.NotificationSender, error) {
	return fakeSender{accounts: f, accountID: accountID}, nil
}

type fakeSender struct {
	accounts  *fakeAccounts
	accountID int64
}

func (s fakeSender) CheckNotificationChat(context.Context, int64) error {
	s.accounts.checked = append(s.accounts.checked, s.accountID)
	return nil
}

func (fakeSender) SendNotification(context.Context, int64, string, string) (int, error) {
	return 1, nil
}

func testWebServer(t *testing.T) (*db.Store, *fakeAccounts, config.Config, http.Handler) {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/web.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := config.Config{BotToken: "123456:test-token", PublicBaseURL: "http://example.test"}
	accounts := &fakeAccounts{}
	server, err := New(cfg, store, accounts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, accounts, cfg, server.routes()
}

func webSession(t *testing.T, handler http.Handler, cfg config.Config, userID int64) *http.Cookie {
	t.Helper()
	response := authenticateWebAppRequest(t, handler, cfg.BotToken, userID, time.Now().UTC())
	if response.Code != http.StatusNoContent {
		t.Fatalf("auth for %d status = %d: %s", userID, response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("auth cookies = %d, want 1", len(cookies))
	}
	return cookies[0]
}

func do(handler http.Handler, method, path, form string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(form))
	if form != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestDashboardRequiresTelegramSession(t *testing.T) {
	store, _, cfg, handler := testWebServer(t)
	ctx := context.Background()
	if _, err := store.AddKeyword(ctx, aliceID, "private dashboard phrase"); err != nil {
		t.Fatal(err)
	}

	unauthenticated := do(handler, http.MethodGet, "/", "", nil)
	if unauthenticated.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", unauthenticated.Code, http.StatusOK)
	}
	if body := unauthenticated.Body.String(); !strings.Contains(body, "Open the dashboard from the telegram-bridge bot") || strings.Contains(body, "private dashboard phrase") {
		t.Fatalf("unauthenticated dashboard body leaked data or missed bootstrap: %q", body)
	}
	if !strings.Contains(unauthenticated.Body.String(), "window.location.reload()") {
		t.Fatal("authentication bootstrap does not preserve rule deep-links")
	}

	for _, path := range []string{
		"/keywords/add", "/rules/add", "/keywords/delete", "/sources/sync", "/sources/toggle", "/history/backfill",
		"/tokens/create", "/tokens/delete", "/notification-chats/add", "/notification-chats/delete",
	} {
		if mutation := do(handler, http.MethodPost, path, "id=1", nil); mutation.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated POST %s status = %d, want %d", path, mutation.Code, http.StatusUnauthorized)
		}
	}

	alice := webSession(t, handler, cfg, aliceID)
	if !alice.HttpOnly || alice.SameSite != http.SameSiteStrictMode {
		t.Fatalf("auth cookie flags = %#v", alice)
	}
	authenticated := do(handler, http.MethodGet, "/", "", alice)
	if authenticated.Code != http.StatusOK || !strings.Contains(authenticated.Body.String(), "private dashboard phrase") {
		t.Fatalf("authenticated dashboard status=%d body=%q", authenticated.Code, authenticated.Body.String())
	}
	if !strings.Contains(authenticated.Body.String(), `id="rule-1"`) {
		t.Fatalf("authenticated dashboard has no rule anchor: %q", authenticated.Body.String())
	}

	stale := authenticateWebAppRequest(t, handler, cfg.BotToken, aliceID, time.Now().Add(-webInitDataMaxAge-time.Second))
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("stale auth status = %d, want %d", stale.Code, http.StatusUnauthorized)
	}
	tamperedBody := url.Values{"init_data": {signedTelegramInitData(cfg.BotToken, map[string]string{
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
		"user":      `{"id":42}`,
	}) + "x"}}.Encode()
	if tampered := do(handler, http.MethodPost, "/webapp/auth", tamperedBody, nil); tampered.Code != http.StatusUnauthorized {
		t.Fatalf("tampered auth status = %d, want %d", tampered.Code, http.StatusUnauthorized)
	}
	forged := &http.Cookie{Name: webAuthCookieName, Value: encodeWebSession("42:"+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10), "other-bot-token")}
	if body := do(handler, http.MethodGet, "/", "", forged).Body.String(); strings.Contains(body, "private dashboard phrase") {
		t.Fatal("a session signed with another key opened the dashboard")
	}
}

func TestDashboardIsolatesUsers(t *testing.T) {
	store, accounts, cfg, handler := testWebServer(t)
	ctx := context.Background()
	now := time.Now()

	aliceRule, err := store.AddKeyword(ctx, aliceID, "alice secret rule")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMonitorPeer(ctx, db.MonitorPeer{OwnerUserID: aliceID, PeerType: "channel", PeerID: 500, Title: "Alice Private Channel", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorPeerEnabled(ctx, aliceID, "channel", 500, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordEvent(ctx, db.Event{OwnerUserID: aliceID, SourcePeerType: "channel", SourcePeerID: 500, MessageID: 1, Keyword: "alice secret rule", Text: "alice secret message"}); err != nil {
		t.Fatal(err)
	}
	_, hash, err := apitoken.New(db.APITokenScopeMCP)
	if err != nil {
		t.Fatal(err)
	}
	aliceToken, err := store.CreateAPIToken(ctx, aliceID, db.APITokenScopeMCP, "alice laptop", hash, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddNotificationChat(ctx, aliceID, -1001234567890, "Alice Group", now); err != nil {
		t.Fatal(err)
	}

	bob := webSession(t, handler, cfg, bobID)
	body := do(handler, http.MethodGet, "/", "", bob).Body.String()
	for _, secret := range []string{"alice secret rule", "Alice Private Channel", "alice secret message", "alice laptop", "Alice Group", "-1001234567890"} {
		if strings.Contains(body, secret) {
			t.Fatalf("bob's dashboard shows alice's %q", secret)
		}
	}

	do(handler, http.MethodPost, "/keywords/delete", "id="+strconv.FormatInt(aliceRule.ID, 10), bob)
	do(handler, http.MethodPost, "/keywords/delete", "id=alice+secret+rule", bob)
	if rules, _ := store.ListKeywords(ctx, aliceID); len(rules) != 1 {
		t.Fatalf("bob deleted alice's rule: %v", rules)
	}
	do(handler, http.MethodPost, "/sources/toggle", "peer_type=channel&peer_id=500&enabled=0", bob)
	if enabled, _ := store.IsMonitorPeerEnabled(ctx, aliceID, "channel", 500); !enabled {
		t.Fatal("bob paused alice's source")
	}
	do(handler, http.MethodPost, "/tokens/delete", "id="+strconv.FormatInt(aliceToken.ID, 10), bob)
	if tokens, _ := store.ListAPITokens(ctx, aliceID); len(tokens) != 1 {
		t.Fatal("bob deleted alice's token")
	}
	do(handler, http.MethodPost, "/notification-chats/delete", "chat_id=-1001234567890", bob)
	if allowed, _ := store.IsNotificationChatAllowed(ctx, aliceID, -1001234567890); !allowed {
		t.Fatal("bob removed alice's notification group")
	}

	// A rule scoped to alice's source is saved for bob without that source.
	do(handler, http.MethodPost, "/rules/add", "name=bob+rule&any=bike&sources=channel:500", bob)
	bobRules, err := store.ListKeywords(ctx, bobID)
	if err != nil || len(bobRules) != 1 || len(bobRules[0].Sources) != 0 {
		t.Fatalf("bob rules = %#v err=%v", bobRules, err)
	}

	// Adding a group is checked with bob's own account and stored for bob only.
	do(handler, http.MethodPost, "/notification-chats/add", "chat=channel:777", bob)
	if len(accounts.checked) != 1 || accounts.checked[0] != bobID {
		t.Fatalf("group checked with accounts %v", accounts.checked)
	}
	if chats, _ := store.ListNotificationChats(ctx, bobID); len(chats) != 1 {
		t.Fatalf("bob chats = %v", chats)
	}
	if chats, _ := store.ListNotificationChats(ctx, aliceID); len(chats) != 1 {
		t.Fatalf("alice chats changed: %v", chats)
	}

	created := do(handler, http.MethodPost, "/tokens/create", "scope=notify&name=bob+script", bob)
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), "tbn_") {
		t.Fatalf("token create status=%d", created.Code)
	}
	if tokens, _ := store.ListAPITokens(ctx, bobID); len(tokens) != 1 || tokens[0].Scope != db.APITokenScopeNotify {
		t.Fatalf("bob tokens = %v", tokens)
	}
	if again := do(handler, http.MethodGet, "/", "", bob).Body.String(); strings.Contains(again, "tbn_") {
		t.Fatal("a created token is shown more than once")
	}
}

func TestParseNotificationChat(t *testing.T) {
	for input, want := range map[string]int64{
		"channel:123":    -1000000000123,
		"-1000000000123": -1000000000123,
		"chat:55":        -55,
		"-55":            -55,
	} {
		if got, err := parseNotificationChat(input); err != nil || got != want {
			t.Fatalf("parseNotificationChat(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "user:5", "5", "abc", "channel:-1"} {
		if _, err := parseNotificationChat(input); err == nil {
			t.Fatalf("parseNotificationChat(%q) accepted", input)
		}
	}
}

func TestDashboardSessionRejectsTamperingAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	valid := encodeWebSession("42:"+strconv.FormatInt(now.Add(time.Hour).Unix(), 10), "bot-token")
	if got, err := decodeWebSession(valid, "bot-token", now); err != nil || got != 42 {
		t.Fatalf("valid session: user=%d err=%v", got, err)
	}
	if _, err := decodeWebSession(valid+"x", "bot-token", now); err == nil {
		t.Fatal("tampered session was accepted")
	}
	expired := encodeWebSession("42:"+strconv.FormatInt(now.Add(-time.Second).Unix(), 10), "bot-token")
	if _, err := decodeWebSession(expired, "bot-token", now); err == nil {
		t.Fatal("expired session was accepted")
	}
}

func authenticateWebAppRequest(t *testing.T, handler http.Handler, botToken string, userID int64, authTime time.Time) *httptest.ResponseRecorder {
	t.Helper()
	initData := signedTelegramInitData(botToken, map[string]string{
		"auth_date": strconv.FormatInt(authTime.Unix(), 10),
		"query_id":  "test-query",
		"user":      `{"id":` + strconv.FormatInt(userID, 10) + `,"first_name":"Test"}`,
	})
	return do(handler, http.MethodPost, "/webapp/auth", url.Values{"init_data": {initData}}.Encode(), nil)
}

func signedTelegramInitData(botToken string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	values := make(url.Values, len(fields)+1)
	for _, key := range keys {
		lines = append(lines, key+"="+fields[key])
		values.Set(key, fields[key])
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secret.Write([]byte(botToken))
	signature := hmac.New(sha256.New, secret.Sum(nil))
	_, _ = signature.Write([]byte(strings.Join(lines, "\n")))
	values.Set("hash", hex.EncodeToString(signature.Sum(nil)))
	return values.Encode()
}
