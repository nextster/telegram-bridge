package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/nextster/telegram-bridge/internal/db"
)

// Telegram Login (OpenID Connect) approves an authorization request for the
// Telegram user who signs in in the browser that opened it.
const (
	telegramIssuer   = "https://oauth.telegram.org"
	telegramAuthURL  = "https://oauth.telegram.org/auth"
	telegramTokenURL = "https://oauth.telegram.org/token"
	telegramJWKSURL  = "https://oauth.telegram.org/.well-known/jwks.json"

	browserCookiePrefix = "tb_oauth_"
	telegramCallback    = "/oauth/telegram/callback"
)

// TelegramLogin configures sign-in with Telegram. ClientID is the bot ID and
// ClientSecret comes from the Login Widget settings of the bot in BotFather.
type TelegramLogin struct {
	ClientID     string
	ClientSecret string
	// The fields below default to Telegram's endpoints; tests replace them.
	AuthURL    string
	TokenURL   string
	KeySet     oidc.KeySet
	HTTPClient *http.Client
}

type telegramLogin struct {
	clientID     string
	clientSecret string
	authURL      string
	tokenURL     string
	verifier     *oidc.IDTokenVerifier
	httpClient   *http.Client
}

func newTelegramLogin(config TelegramLogin) (*telegramLogin, error) {
	clientID := strings.TrimSpace(config.ClientID)
	if id, err := strconv.ParseInt(clientID, 10, 64); err != nil || id <= 0 || strings.TrimSpace(config.ClientSecret) == "" {
		return nil, errors.New("telegram login needs the bot ID and the Login Widget client secret")
	}
	login := &telegramLogin{
		clientID:     clientID,
		clientSecret: strings.TrimSpace(config.ClientSecret),
		authURL:      config.AuthURL,
		tokenURL:     config.TokenURL,
		httpClient:   config.HTTPClient,
	}
	if login.authURL == "" {
		login.authURL = telegramAuthURL
	}
	if login.tokenURL == "" {
		login.tokenURL = telegramTokenURL
	}
	if login.httpClient == nil {
		login.httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	keySet := config.KeySet
	if keySet == nil {
		keySet = oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), login.httpClient), telegramJWKSURL)
	}
	login.verifier = oidc.NewVerifier(telegramIssuer, keySet, &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.EdDSA},
	})
	return login, nil
}

func browserCookieName(requestID string) string {
	return browserCookiePrefix + requestID
}

// setBrowserCookie remembers the browser secret of a request across the
// redirect to Telegram. Lax lets it return with Telegram's top-level redirect.
func (s *Server) setBrowserCookie(w http.ResponseWriter, requestID, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     browserCookieName(requestID),
		Value:    secret,
		Path:     "/oauth/",
		MaxAge:   int(requestTTL.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.issuer, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearBrowserCookie(w http.ResponseWriter, requestID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     browserCookieName(requestID),
		Value:    "",
		Path:     "/oauth/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.issuer, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// browserRequest loads a pending request that was opened in this browser.
func (s *Server) browserRequest(w http.ResponseWriter, r *http.Request, requestID string) (requestView, bool) {
	cookie, err := r.Cookie(browserCookieName(requestID))
	if requestID == "" || err != nil {
		s.renderError(w, http.StatusBadRequest, "Эта ссылка открыта не в том браузере, где начиналось подключение. Если её вам прислали, закройте страницу: так пытаются получить доступ к чужому Telegram.")
		return requestView{}, false
	}
	request, ok, err := s.store.GetOAuthRequest(r.Context(), requestID)
	if err != nil {
		log.Printf("oauth request lookup failed: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Не удалось проверить запрос. Попробуйте ещё раз.")
		return requestView{}, false
	}
	if !ok || !secretMatches(cookie.Value, request.BrowserHash) {
		s.renderError(w, http.StatusBadRequest, "Эта ссылка открыта не в том браузере, где начиналось подключение. Если её вам прислали, закройте страницу: так пытаются получить доступ к чужому Telegram.")
		return requestView{}, false
	}
	if request.Status != db.OAuthRequestPending || !s.now().Before(request.ExpiresAt) {
		s.renderError(w, http.StatusBadRequest, "Запрос на подключение устарел. Запустите подключение заново.")
		return requestView{}, false
	}
	return requestView{request: request, secret: cookie.Value}, true
}

// telegramStart redirects the browser that opened a request to Telegram.
func (s *Server) telegramStart(w http.ResponseWriter, r *http.Request) {
	view, ok := s.browserRequest(w, r, r.URL.Query().Get("request"))
	if !ok {
		return
	}
	if !s.allowRequest("telegram-login", r) {
		s.renderError(w, http.StatusTooManyRequests, "Слишком много попыток входа. Подождите немного и попробуйте снова.")
		return
	}
	state := randomToken(32)
	nonce := randomToken(32)
	verifier := randomToken(48)
	started, err := s.store.StartOAuthTelegramLogin(r.Context(), view.request.ID, hashSecret(state), nonce, verifier, s.now())
	if err != nil || !started {
		if err != nil {
			log.Printf("oauth telegram login start failed: %v", err)
		}
		s.renderError(w, http.StatusBadRequest, "Запрос на подключение устарел. Запустите подключение заново.")
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id":             {s.telegram.clientID},
		"redirect_uri":          {s.issuer + telegramCallback},
		"response_type":         {"code"},
		"scope":                 {"openid profile"},
		"state":                 {view.request.ID + "." + state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, s.telegram.authURL+"?"+query.Encode(), http.StatusFound)
}

// telegramCallback finishes the sign-in, approves the request for the
// signed-in user, and returns the browser to the client with a code.
func (s *Server) telegramCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	requestID, state, _ := strings.Cut(query.Get("state"), ".")
	view, ok := s.browserRequest(w, r, requestID)
	if !ok {
		return
	}
	ctx := r.Context()
	nonce, verifier, found, err := s.store.FinishOAuthTelegramLogin(ctx, view.request.ID, hashSecret(state), s.now())
	if err != nil {
		log.Printf("oauth telegram login finish failed: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Не удалось завершить вход. Попробуйте ещё раз.")
		return
	}
	if !found || state == "" {
		s.renderError(w, http.StatusBadRequest, "Вход через Telegram устарел или уже использован. Запустите подключение заново.")
		return
	}
	if query.Get("error") != "" || query.Get("code") == "" {
		if _, _, err := s.store.DecideOAuthRequest(ctx, view.request.ID, false, 0, s.now()); err != nil {
			log.Printf("oauth request deny failed: %v", err)
		}
		s.clearBrowserCookie(w, view.request.ID)
		http.Redirect(w, r, s.errorRedirect(view.request.RedirectURI, view.request.State, "access_denied", "Telegram sign-in was cancelled"), http.StatusSeeOther)
		return
	}
	userID, err := s.telegram.userID(ctx, query.Get("code"), s.issuer+telegramCallback, verifier, nonce)
	if err != nil {
		log.Printf("oauth telegram login failed: %v", err)
		s.renderError(w, http.StatusBadGateway, "Telegram не подтвердил вход. Попробуйте ещё раз.")
		return
	}
	connected, err := s.approver.OAuthAccountConnected(ctx, userID)
	if err != nil {
		log.Printf("oauth account check failed: %v", err)
		s.renderError(w, http.StatusServiceUnavailable, "Не удалось проверить аккаунт. Попробуйте ещё раз.")
		return
	}
	if !connected {
		link, err := s.approver.OAuthBotLink(ctx)
		if err != nil {
			log.Printf("oauth bot link failed: %v", err)
		}
		s.writePage(w, http.StatusForbidden, approvalPage{
			Error:   "Сначала подключите свой Telegram: отправьте /login боту, затем запустите подключение заново.",
			BotLink: link,
		})
		return
	}
	now := s.now()
	if _, approved, err := s.store.DecideOAuthRequest(ctx, view.request.ID, true, userID, now); err != nil || !approved {
		if err != nil {
			log.Printf("oauth request approve failed: %v", err)
		}
		s.renderError(w, http.StatusBadRequest, "Запрос на подключение устарел. Запустите подключение заново.")
		return
	}
	code := randomToken(32)
	if issued, err := s.store.IssueOAuthCode(ctx, view.request.ID, hashSecret(code), now.Add(codeTTL), now); err != nil || !issued {
		if err != nil {
			log.Printf("oauth code issue failed: %v", err)
		}
		s.renderError(w, http.StatusInternalServerError, "Не удалось завершить подключение. Запустите его заново.")
		return
	}
	s.clearBrowserCookie(w, view.request.ID)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.successRedirect(view.request.RedirectURI, view.request.State, code), http.StatusSeeOther)
}

type requestView struct {
	request db.OAuthRequest
	secret  string
}

// userID exchanges the authorization code and returns the Telegram user ID
// from the verified ID token.
func (l *telegramLogin) userID(ctx context.Context, code, redirectURI, verifier, nonce string) (int64, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {l.clientID},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(l.clientID, l.clientSecret)
	response, err := l.httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("telegram token request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return 0, fmt.Errorf("read telegram token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("telegram token endpoint returned %d", response.StatusCode)
	}
	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil || tokens.IDToken == "" {
		return 0, errors.New("telegram token response has no id_token")
	}
	idToken, err := l.verifier.Verify(oidc.ClientContext(ctx, l.httpClient), tokens.IDToken)
	if err != nil {
		return 0, fmt.Errorf("verify telegram id_token: %w", err)
	}
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return 0, errors.New("telegram id_token nonce does not match")
	}
	// The numeric user ID is the "id" claim of the profile scope; "sub" is a
	// different identifier.
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return 0, fmt.Errorf("decode telegram id_token claims: %w", err)
	}
	userID, ok := telegramUserID(claims["id"])
	if !ok {
		return 0, fmt.Errorf("telegram id_token has no user id; claims: %s", claimShape(claims))
	}
	return userID, nil
}

// telegramUserID accepts the user ID as a JSON number or a numeric string.
func telegramUserID(raw json.RawMessage) (int64, bool) {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

// claimShape lists claim names and JSON types without their values, so a
// failed sign-in can be diagnosed without logging personal data.
func claimShape(claims map[string]json.RawMessage) string {
	shape := make([]string, 0, len(claims))
	for name, raw := range claims {
		kind := "null"
		switch value := strings.TrimSpace(string(raw)); {
		case strings.HasPrefix(value, `"`):
			kind = "string"
		case strings.HasPrefix(value, "{"):
			kind = "object"
		case strings.HasPrefix(value, "["):
			kind = "array"
		case value == "true" || value == "false":
			kind = "bool"
		case value != "null":
			kind = "number"
		}
		shape = append(shape, name+":"+kind)
	}
	slices.Sort(shape)
	return strings.Join(shape, ",")
}
