package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	webAuthCookieName = "telegram_bridge_session"
	webAuthMaxAge     = 24 * time.Hour
	webInitDataMaxAge = 10 * time.Minute
)

type telegramWebAppUser struct {
	ID int64 `json:"id"`
}

func (s *Server) dashboardEntry(w http.ResponseWriter, r *http.Request) {
	if userID, ok := s.authenticatedWebUser(r); ok {
		s.dashboard(w, r, userID)
		return
	}
	renderWebAppBootstrap(w)
}

// requireWebUser passes the signed-in Telegram user to next. Handlers must
// scope every read and write to that user.
func (s *Server) requireWebUser(next func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		userID, ok := s.authenticatedWebUser(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r, userID)
	}
}

func (s *Server) authenticateWebApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// A cross-site form could otherwise sign a browser in as another user.
	if !sameOriginRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid authentication payload", http.StatusBadRequest)
		return
	}
	userID, err := validateTelegramWebAppInitData(r.FormValue("init_data"), s.cfg.BotToken, time.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	expires := time.Now().Add(webAuthMaxAge)
	payload := fmt.Sprintf("%d:%d", userID, expires.Unix())
	http.SetCookie(w, &http.Cookie{
		Name:     webAuthCookieName,
		Value:    encodeWebSession(payload, s.cfg.BotToken),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(webAuthMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r, s.cfg.PublicBaseURL),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticatedWebUser(r *http.Request) (int64, bool) {
	cookie, err := r.Cookie(webAuthCookieName)
	if err != nil {
		return 0, false
	}
	userID, err := decodeWebSession(cookie.Value, s.cfg.BotToken, time.Now())
	if err != nil || userID <= 0 {
		return 0, false
	}
	return userID, true
}

func validateTelegramWebAppInitData(raw, botToken string, now time.Time) (int64, error) {
	raw = strings.TrimSpace(raw)
	botToken = strings.TrimSpace(botToken)
	if raw == "" || botToken == "" {
		return 0, errors.New("missing Telegram Mini App authentication")
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return 0, fmt.Errorf("parse Telegram Mini App data: %w", err)
	}
	hashValues := values["hash"]
	if len(hashValues) != 1 {
		return 0, errors.New("Telegram Mini App hash is missing or duplicated")
	}
	receivedHash, err := hex.DecodeString(hashValues[0])
	if err != nil || len(receivedHash) != sha256.Size {
		return 0, errors.New("Telegram Mini App hash is invalid")
	}
	delete(values, "hash")

	keys := make([]string, 0, len(values))
	for key, entries := range values {
		if len(entries) != 1 {
			return 0, fmt.Errorf("Telegram Mini App field %q is duplicated", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values.Get(key))
	}

	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secretMAC.Write([]byte(botToken))
	signatureMAC := hmac.New(sha256.New, secretMAC.Sum(nil))
	_, _ = signatureMAC.Write([]byte(strings.Join(lines, "\n")))
	if !hmac.Equal(receivedHash, signatureMAC.Sum(nil)) {
		return 0, errors.New("Telegram Mini App signature does not match")
	}

	authUnix, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || authUnix <= 0 {
		return 0, errors.New("Telegram Mini App auth date is invalid")
	}
	age := now.Unix() - authUnix
	if age < -60 || age > int64(webInitDataMaxAge.Seconds()) {
		return 0, errors.New("Telegram Mini App authentication is stale")
	}

	var user telegramWebAppUser
	if err := json.Unmarshal([]byte(values.Get("user")), &user); err != nil || user.ID <= 0 {
		return 0, errors.New("Telegram Mini App user is invalid")
	}
	return user.ID, nil
}

func encodeWebSession(payload, botToken string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + hex.EncodeToString(webSessionMAC(encoded, botToken))
}

func decodeWebSession(value, botToken string, now time.Time) (int64, error) {
	encoded, signature, ok := strings.Cut(strings.TrimSpace(value), ".")
	if !ok || encoded == "" || signature == "" || strings.TrimSpace(botToken) == "" {
		return 0, errors.New("invalid dashboard session")
	}
	receivedMAC, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(receivedMAC, webSessionMAC(encoded, botToken)) {
		return 0, errors.New("invalid dashboard session signature")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, errors.New("invalid dashboard session payload")
	}
	rawUserID, rawExpiry, ok := strings.Cut(string(payloadBytes), ":")
	if !ok {
		return 0, errors.New("invalid dashboard session payload")
	}
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err != nil || userID <= 0 {
		return 0, errors.New("invalid dashboard session user")
	}
	expiresUnix, err := strconv.ParseInt(rawExpiry, 10, 64)
	if err != nil || expiresUnix <= now.Unix() {
		return 0, errors.New("dashboard session expired")
	}
	return userID, nil
}

func webSessionMAC(encoded, botToken string) []byte {
	mac := hmac.New(sha256.New, []byte(botToken))
	_, _ = mac.Write([]byte("telegram-bridge-web-session\n" + encoded))
	return mac.Sum(nil)
}

// sameOriginRequest rejects requests that a browser marks as cross-site or
// that carry a foreign Origin. Clients that send neither header are not
// browsers acting on another site's behalf.
func sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == r.Host
}

func requestUsesHTTPS(r *http.Request, publicBaseURL string) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(publicBaseURL)), "https://")
}

func renderWebAppBootstrap(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src https://telegram.org 'unsafe-inline'; connect-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors https://web.telegram.org https://*.telegram.org")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(webAppBootstrapPage))
}

const webAppBootstrapPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>telegram-bridge</title>
  <style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#202124}code{background:#f2f3f5;padding:.15rem .35rem;border-radius:.3rem}</style>
  <script src="https://telegram.org/js/telegram-web-app.js"></script>
</head>
<body>
  <h1>telegram-bridge</h1>
  <p id="status">Authenticating Telegram Mini App…</p>
  <script>
    (async () => {
      const status = document.getElementById('status');
      const app = window.Telegram && window.Telegram.WebApp;
      const initData = app && app.initData;
      if (!initData) {
        status.textContent = 'Open the dashboard from the telegram-bridge bot.';
        return;
      }
      app.ready();
      try {
        const body = new URLSearchParams({init_data: initData});
        const response = await fetch('/webapp/auth', {
          method: 'POST',
          credentials: 'same-origin',
          headers: {'Content-Type': 'application/x-www-form-urlencoded'},
          body,
        });
        if (!response.ok) throw new Error('authentication rejected');
		window.location.reload();
      } catch (_) {
        status.textContent = 'Dashboard access was rejected. Open it again from the bot.';
      }
    })();
  </script>
</body>
</html>`
