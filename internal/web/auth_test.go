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

	"github.com/nextster/tg-radar/internal/config"
	"github.com/nextster/tg-radar/internal/db"
)

func TestDashboardRequiresTelegramAdminSession(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/web-auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertSubscriber(ctx, db.Subscriber{ChatID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddKeyword(ctx, "private dashboard phrase"); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{BotToken: "123456:test-token", PublicBaseURL: "http://example.test"}
	server, err := New(cfg, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.routes()

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/", nil))
	if unauthenticated.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", unauthenticated.Code, http.StatusOK)
	}
	if body := unauthenticated.Body.String(); !strings.Contains(body, "Open the dashboard from the tg-radar bot admin chat") || strings.Contains(body, "private dashboard phrase") {
		t.Fatalf("unauthenticated dashboard body leaked data or missed bootstrap: %q", body)
	}
	if !strings.Contains(unauthenticated.Body.String(), "window.location.reload()") {
		t.Fatal("authentication bootstrap does not preserve rule deep-links")
	}

	for _, path := range []string{
		"/keywords/add", "/rules/add", "/keywords/delete", "/sources/sync", "/sources/toggle", "/history/backfill",
	} {
		mutation := httptest.NewRecorder()
		handler.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, path, strings.NewReader("id=1")))
		if mutation.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated POST %s status = %d, want %d", path, mutation.Code, http.StatusUnauthorized)
		}
	}

	now := time.Now().UTC()
	authResponse := authenticateWebAppRequest(t, handler, cfg.BotToken, 42, now)
	if authResponse.Code != http.StatusNoContent {
		t.Fatalf("admin auth status = %d, want %d: %s", authResponse.Code, http.StatusNoContent, authResponse.Body.String())
	}
	cookies := authResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("auth cookies = %d, want 1", len(cookies))
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("auth cookie flags = %#v", cookies[0])
	}

	authenticatedRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	authenticatedRequest.AddCookie(cookies[0])
	authenticated := httptest.NewRecorder()
	handler.ServeHTTP(authenticated, authenticatedRequest)
	if authenticated.Code != http.StatusOK || !strings.Contains(authenticated.Body.String(), "private dashboard phrase") {
		t.Fatalf("authenticated dashboard status=%d body=%q", authenticated.Code, authenticated.Body.String())
	}
	if !strings.Contains(authenticated.Body.String(), `id="rule-1"`) {
		t.Fatalf("authenticated dashboard has no rule anchor: %q", authenticated.Body.String())
	}

	nonAdmin := authenticateWebAppRequest(t, handler, cfg.BotToken, 99, now)
	if nonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin auth status = %d, want %d", nonAdmin.Code, http.StatusForbidden)
	}
	stale := authenticateWebAppRequest(t, handler, cfg.BotToken, 42, now.Add(-webInitDataMaxAge-time.Second))
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("stale auth status = %d, want %d", stale.Code, http.StatusUnauthorized)
	}

	tamperedBody := url.Values{"init_data": {signedTelegramInitData(cfg.BotToken, map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"user":      `{"id":42}`,
	}) + "x"}}.Encode()
	tamperedRequest := httptest.NewRequest(http.MethodPost, "/webapp/auth", strings.NewReader(tamperedBody))
	tamperedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tampered := httptest.NewRecorder()
	handler.ServeHTTP(tampered, tamperedRequest)
	if tampered.Code != http.StatusUnauthorized {
		t.Fatalf("tampered auth status = %d, want %d", tampered.Code, http.StatusUnauthorized)
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
	body := url.Values{"init_data": {initData}}.Encode()
	request := httptest.NewRequest(http.MethodPost, "/webapp/auth", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
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
