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
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/nextster/telegram-bridge/internal/db"
)

// Telegram Login (OpenID Connect) binds an authorization request to the
// Telegram user signed in to the browser that opened it. Without it, anyone
// could open a request and talk another user into approving it in the bot.
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

// telegramCallback finishes the sign-in, binds the request to the signed-in
// user, and asks that user to approve it in the bot.
func (s *Server) telegramCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	requestID, state, _ := strings.Cut(query.Get("state"), ".")
	view, ok := s.browserRequest(w, r, requestID)
	if !ok {
		return
	}
	nonce, verifier, found, err := s.store.FinishOAuthTelegramLogin(r.Context(), view.request.ID, hashSecret(state), s.now())
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
		s.renderError(w, http.StatusBadRequest, "Вход через Telegram отменён. Запустите подключение заново.")
		return
	}
	userID, err := s.telegram.userID(r.Context(), query.Get("code"), s.issuer+telegramCallback, verifier, nonce)
	if err != nil {
		log.Printf("oauth telegram login failed: %v", err)
		s.renderError(w, http.StatusBadGateway, "Telegram не подтвердил вход. Попробуйте ещё раз.")
		return
	}
	bound, err := s.store.BindOAuthRequest(r.Context(), view.request.ID, userID, s.now())
	if err != nil || !bound {
		if err != nil {
			log.Printf("oauth request bind failed: %v", err)
		}
		s.renderError(w, http.StatusBadRequest, "Запрос на подключение устарел. Запустите подключение заново.")
		return
	}
	if err := s.approver.OAuthApprovalRequested(context.WithoutCancel(r.Context()), userID, view.request.ID); err != nil {
		log.Printf("oauth approval prompt failed: %v", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/oauth/authorize/continue?request="+url.QueryEscape(view.request.ID), http.StatusSeeOther)
}

// authorizeContinue shows the approval step to the browser that signed in.
func (s *Server) authorizeContinue(w http.ResponseWriter, r *http.Request) {
	view, ok := s.browserRequest(w, r, r.URL.Query().Get("request"))
	if !ok {
		return
	}
	if view.request.BoundUserID <= 0 {
		http.Redirect(w, r, "/oauth/telegram/start?request="+url.QueryEscape(view.request.ID), http.StatusSeeOther)
		return
	}
	client, found, err := s.store.GetOAuthClient(r.Context(), view.request.ClientID)
	if err != nil || !found {
		s.renderError(w, http.StatusBadRequest, "Клиент не зарегистрирован. Запустите подключение заново из Claude или Codex.")
		return
	}
	link, err := s.approver.OAuthApprovalLink(r.Context(), view.request.ID)
	if err != nil {
		log.Printf("oauth approval link failed: %v", err)
		s.renderError(w, http.StatusServiceUnavailable, "Telegram-бот недоступен. Попробуйте позже.")
		return
	}
	s.renderApproval(w, approvalPage{
		ClientName:    displayClientName(client.Name),
		Code:          view.request.ApprovalCode,
		ApprovalLink:  link,
		RequestID:     view.request.ID,
		BrowserSecret: view.secret,
		SignedIn:      true,
	})
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
	var claims struct {
		ID int64 `json:"id"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.ID <= 0 {
		return 0, errors.New("telegram id_token has no user id")
	}
	return claims.ID, nil
}
