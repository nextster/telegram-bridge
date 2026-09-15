// Package oauth implements the OAuth 2.1 authorization server that lets MCP
// clients such as Claude and Codex connect without a shared bearer token.
//
// A request is first bound to the Telegram user who signs in with Telegram in
// the browser that opened it, so nobody can talk another user into approving a
// request they opened themselves. The bot then asks that user, and only that
// user, to pick the number shown on the page. Codes and tokens never pass
// through Telegram; only SHA-256 hashes of them are stored.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nextster/telegram-bridge/internal/db"
)

const (
	Scope = "telegram"

	accessTokenTTL  = time.Hour
	refreshTokenTTL = 90 * 24 * time.Hour
	requestTTL      = 10 * time.Minute
	codeTTL         = 2 * time.Minute

	maxClients         = 500
	maxPendingRequests = 20
	perIPPerHour       = 20
	globalPerHour      = 200
	maxLimiterKeys     = 10000

	refreshReuseGrace = 30 * time.Second
	maxGrantLifetime  = 365 * 24 * time.Hour

	accessTokenPrefix  = "tba_"
	refreshTokenPrefix = "tbr_"
	clientIDPrefix     = "tbc_"

	authMethodNone   = "none"
	authMethodBasic  = "client_secret_basic"
	authMethodPost   = "client_secret_post"
	maxRedirectURIs  = 10
	maxClientNameLen = 60
	maxUserAgentLen  = 160
)

var refreshPolicy = db.OAuthRefreshPolicy{ReuseGrace: refreshReuseGrace, MaxGrantLifetime: maxGrantLifetime}

// Redirect URIs of hosted clients whose callbacks bind the result to the
// signed-in user's own account. Loopback redirects are always accepted.
var trustedRedirectURIs = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
}

type Store interface {
	CreateOAuthClient(context.Context, db.OAuthClient, int, time.Time) error
	GetOAuthClient(context.Context, string) (db.OAuthClient, bool, error)
	CreateOAuthRequest(context.Context, db.OAuthRequest, int, time.Time) error
	GetOAuthRequest(context.Context, string) (db.OAuthRequest, bool, error)
	GetOAuthRequestByCode(context.Context, string) (db.OAuthRequest, bool, error)
	StartOAuthTelegramLogin(context.Context, string, string, string, string, time.Time) (bool, error)
	FinishOAuthTelegramLogin(context.Context, string, string, time.Time) (string, string, bool, error)
	BindOAuthRequest(context.Context, string, int64, time.Time) (bool, error)
	IssueOAuthCode(context.Context, string, string, time.Time, time.Time) (bool, error)
	ExchangeOAuthCode(context.Context, string, string, db.OAuthGrant, []db.OAuthToken, time.Time) error
	RefreshOAuthTokens(context.Context, string, string, []db.OAuthToken, db.OAuthRefreshPolicy, time.Time) (db.OAuthGrant, error)
	VerifyOAuthAccessToken(context.Context, string, time.Time) (db.OAuthGrant, time.Time, bool, error)
	RevokeOAuthToken(context.Context, string, string, time.Time) error
}

// Approver is the Telegram side of the flow.
type Approver interface {
	// OAuthApprovalLink returns a link that opens the approval prompt for the
	// request in the bot.
	OAuthApprovalLink(ctx context.Context, requestID string) (string, error)
	// OAuthApprovalRequested sends the approval prompt of a request to the
	// user it is bound to.
	OAuthApprovalRequested(ctx context.Context, userID int64, requestID string) error
	// OAuthConnectionRevoked tells the user that one of their connections was
	// cut off.
	OAuthConnectionRevoked(ctx context.Context, userID int64, clientName, reason string) error
	// OAuthConnectionCreated tells the user that a client now has access to
	// their account, so a connection they did not start can be revoked at once.
	OAuthConnectionCreated(ctx context.Context, userID int64, clientName, clientIP string) error
}

type Options struct {
	ExtraRedirectURIs []string
	TelegramLogin     TelegramLogin
	Now               func() time.Time
}

type Server struct {
	issuer       string
	resource     string
	store        Store
	approver     Approver
	extraURIs    []string
	now          func() time.Time
	limiter      *limiter
	pageTemplate pageRenderer
	telegram     *telegramLogin
}

func New(publicURL string, store Store, approver Approver, options Options) (*Server, error) {
	issuer := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname())) || parsed.Path != "" {
		return nil, fmt.Errorf("oauth issuer must be an https origin without a path, got %q", publicURL)
	}
	if store == nil || approver == nil {
		return nil, errors.New("oauth requires a store and a Telegram approver")
	}
	telegram, err := newTelegramLogin(options.TelegramLogin)
	if err != nil {
		return nil, err
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	extra := make([]string, 0, len(options.ExtraRedirectURIs))
	for _, uri := range options.ExtraRedirectURIs {
		if uri = strings.TrimSpace(uri); uri != "" {
			extra = append(extra, uri)
		}
	}
	return &Server{
		issuer:       issuer,
		resource:     issuer + "/mcp",
		store:        store,
		approver:     approver,
		extraURIs:    extra,
		now:          now,
		limiter:      newLimiter(now),
		pageTemplate: newPageRenderer(),
		telegram:     telegram,
	}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.protectedResourceMetadata(s.issuer))
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.protectedResourceMetadata(s.resource))
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.authorizationServerMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("GET /oauth/telegram/start", s.telegramStart)
	mux.HandleFunc("GET "+telegramCallback, s.telegramCallback)
	mux.HandleFunc("GET /oauth/authorize/continue", s.authorizeContinue)
	mux.HandleFunc("POST /oauth/authorize/status", s.authorizeStatus)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
}

// ResourceMetadataURL is advertised in WWW-Authenticate challenges from /mcp.
func (s *Server) ResourceMetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource/mcp"
}

// VerifyAccessToken resolves a live OAuth access token to the Telegram user
// who approved it and the token's expiry.
func (s *Server) VerifyAccessToken(ctx context.Context, token string) (int64, time.Time, bool, error) {
	if !strings.HasPrefix(token, accessTokenPrefix) {
		return 0, time.Time{}, false, nil
	}
	grant, expires, ok, err := s.store.VerifyOAuthAccessToken(ctx, hashSecret(token), s.now())
	if err != nil || !ok || grant.Resource != s.resource || grant.UserID <= 0 {
		return 0, time.Time{}, false, err
	}
	return grant.UserID, expires, true, nil
}

func (s *Server) protectedResourceMetadata(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowMetadataMethod(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":                 resource,
			"authorization_servers":    []string{s.issuer},
			"scopes_supported":         []string{Scope},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "Telegram Bridge",
		})
	}
}

func (s *Server) authorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if !allowMetadataMethod(w, r) {
		return
	}
	authMethods := []string{authMethodNone, authMethodBasic, authMethodPost}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + "/oauth/authorize",
		"token_endpoint":                                 s.issuer + "/oauth/token",
		"registration_endpoint":                          s.issuer + "/oauth/register",
		"revocation_endpoint":                            s.issuer + "/oauth/revoke",
		"scopes_supported":                               []string{Scope},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          authMethods,
		"revocation_endpoint_auth_methods_supported":     authMethods,
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if !s.allowRequest("register", r) {
		writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many client registrations; try again later")
		return
	}
	var request registrationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "registration body must be JSON")
		return
	}
	if len(request.RedirectURIs) == 0 || len(request.RedirectURIs) > maxRedirectURIs {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "provide between 1 and 10 redirect_uris")
		return
	}
	for _, uri := range request.RedirectURIs {
		if !s.allowedRedirectURI(uri) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be a loopback address or a trusted client callback: "+uri)
			return
		}
	}
	method := request.TokenEndpointAuthMethod
	if method == "" {
		method = authMethodBasic
	}
	if !slices.Contains([]string{authMethodNone, authMethodBasic, authMethodPost}, method) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	grantTypes := request.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	for _, grantType := range grantTypes {
		if grantType != "authorization_code" && grantType != "refresh_token" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant type: "+grantType)
			return
		}
	}
	responseTypes := request.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	for _, responseType := range responseTypes {
		if responseType != "code" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response type: "+responseType)
			return
		}
	}

	now := s.now()
	client := db.OAuthClient{
		ClientID:     clientIDPrefix + randomToken(16),
		Name:         cleanClientName(request.ClientName),
		RedirectURIs: request.RedirectURIs,
		AuthMethod:   method,
	}
	secret := ""
	if method != authMethodNone {
		secret = randomToken(32)
		client.SecretHash = hashSecret(secret)
	}
	if err := s.store.CreateOAuthClient(r.Context(), client, maxClients, now); err != nil {
		if errors.Is(err, db.ErrOAuthLimit) {
			writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "client registration limit reached")
			return
		}
		log.Printf("oauth register failed: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	response := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        now.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                grantTypes,
		"response_types":             responseTypes,
	}
	if client.Name != "" {
		response["client_name"] = client.Name
	}
	if secret != "" {
		response["client_secret"] = secret
		response["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	client, ok, err := s.store.GetOAuthClient(r.Context(), query.Get("client_id"))
	if err != nil {
		log.Printf("oauth authorize client lookup failed: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Не удалось проверить клиента. Попробуйте ещё раз.")
		return
	}
	if !ok {
		s.renderError(w, http.StatusBadRequest, "Клиент не зарегистрирован. Запустите подключение заново из Claude или Codex.")
		return
	}
	redirectURI, ok := registeredRedirectURI(client, query.Get("redirect_uri"))
	if !ok || !s.allowedRedirectURI(redirectURI) {
		s.renderError(w, http.StatusBadRequest, "Адрес возврата не совпадает с зарегистрированным. Запустите подключение заново.")
		return
	}
	// Invalid parameters are shown on the page instead of being redirected, so
	// the endpoint cannot be used to bounce requests to arbitrary local URLs.
	if query.Get("response_type") != "code" {
		s.renderError(w, http.StatusBadRequest, "Клиент запросил неподдерживаемый тип ответа.")
		return
	}
	challenge := query.Get("code_challenge")
	if query.Get("code_challenge_method") != "S256" || !validPKCEValue(challenge) {
		s.renderError(w, http.StatusBadRequest, "Клиент не передал PKCE S256. Обновите Claude или Codex.")
		return
	}
	if !s.validResource(query["resource"]) {
		s.renderError(w, http.StatusBadRequest, "Клиент запросил доступ к другому ресурсу.")
		return
	}
	if !s.allowRequest("authorize", r) {
		s.renderError(w, http.StatusTooManyRequests, "Слишком много запросов на подключение. Подождите немного и попробуйте снова.")
		return
	}

	now := s.now()
	browserSecret := randomToken(32)
	request := db.OAuthRequest{
		ID:            randomToken(16),
		BrowserHash:   hashSecret(browserSecret),
		ClientID:      client.ClientID,
		RedirectURI:   redirectURI,
		State:         query.Get("state"),
		CodeChallenge: challenge,
		Resource:      s.resource,
		ApprovalCode:  fmt.Sprintf("%02d", randomInt(90)+10),
		ClientIP:      clientIP(r),
		UserAgent:     truncateRunes(cleanClientName(r.UserAgent()), maxUserAgentLen),
		ExpiresAt:     now.Add(requestTTL),
	}
	if err := s.store.CreateOAuthRequest(r.Context(), request, maxPendingRequests, now); err != nil {
		if errors.Is(err, db.ErrOAuthLimit) {
			s.renderError(w, http.StatusTooManyRequests, "Слишком много незавершённых запросов. Подождите 10 минут и попробуйте снова.")
			return
		}
		log.Printf("oauth authorize request failed: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Не удалось создать запрос. Попробуйте ещё раз.")
		return
	}
	s.setBrowserCookie(w, request.ID, browserSecret)
	s.writePage(w, http.StatusOK, approvalPage{
		ClientName: displayClientName(client.Name),
		SignInLink: "/oauth/telegram/start?request=" + url.QueryEscape(request.ID),
	})
}

type statusRequest struct {
	Request string `json:"request"`
	Secret  string `json:"secret"`
}

func (s *Server) authorizeStatus(w http.ResponseWriter, r *http.Request) {
	var input statusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid"})
		return
	}
	request, ok, err := s.store.GetOAuthRequest(r.Context(), input.Request)
	if err != nil {
		log.Printf("oauth status lookup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
		return
	}
	if !ok || !secretMatches(input.Secret, request.BrowserHash) {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "expired"})
		return
	}
	now := s.now()
	switch request.Status {
	case db.OAuthRequestPending:
		if !now.Before(request.ExpiresAt) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
	case db.OAuthRequestDenied:
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "denied",
			"redirect": s.errorRedirect(request.RedirectURI, request.State, "access_denied", "the Telegram Bridge owner denied access"),
		})
	case db.OAuthRequestApproved:
		code := randomToken(32)
		issued, err := s.store.IssueOAuthCode(r.Context(), request.ID, hashSecret(code), now.Add(codeTTL), now)
		if err != nil {
			log.Printf("oauth code issue failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
			return
		}
		if !issued {
			// Codes are minted once, only while the request is still valid.
			status := "expired"
			if request.CodeHash != "" {
				status = "done"
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": status})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "approved",
			"redirect": s.successRedirect(request.RedirectURI, request.State, code),
		})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token request must be form-encoded")
		return
	}
	client, ok := s.authenticateClient(w, r)
	if !ok {
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, client)
	case "refresh_token":
		s.refresh(w, r, client)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, client db.OAuthClient) {
	form := r.PostForm
	codeHash := hashSecret(form.Get("code"))
	request, ok, err := s.store.GetOAuthRequestByCode(r.Context(), codeHash)
	if err != nil {
		log.Printf("oauth code lookup failed: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not verify the authorization code")
		return
	}
	now := s.now()
	if form.Get("code") == "" || !ok || request.ClientID != client.ClientID || request.Status != db.OAuthRequestApproved || !now.Before(request.CodeExpiresAt) ||
		request.BoundUserID <= 0 || request.DecidedBy != request.BoundUserID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if redirectURI := form.Get("redirect_uri"); redirectURI != "" && redirectURI != request.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if !pkceMatches(form.Get("code_verifier"), request.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match")
		return
	}
	if !s.validResource(form["resource"]) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+s.resource)
		return
	}
	grant := db.OAuthGrant{
		ID:         randomToken(12),
		ClientID:   client.ClientID,
		ClientName: displayClientName(client.Name),
		UserID:     request.DecidedBy,
		Resource:   request.Resource,
	}
	access, refresh, tokens := newTokenPair(now)
	if err := s.store.ExchangeOAuthCode(r.Context(), request.ID, codeHash, grant, tokens, now); err != nil {
		if errors.Is(err, db.ErrOAuthInvalidGrant) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code was already used")
			return
		}
		log.Printf("oauth code exchange failed: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	if err := s.approver.OAuthConnectionCreated(context.WithoutCancel(r.Context()), grant.UserID, grant.ClientName, request.ClientIP); err != nil {
		log.Printf("oauth connection alert failed: %v", err)
	}
	writeTokenResponse(w, access, refresh)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, client db.OAuthClient) {
	refreshToken := r.PostForm.Get("refresh_token")
	if !strings.HasPrefix(refreshToken, refreshTokenPrefix) || !s.validResource(r.PostForm["resource"]) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid")
		return
	}
	now := s.now()
	access, refresh, tokens := newTokenPair(now)
	grant, err := s.store.RefreshOAuthTokens(r.Context(), hashSecret(refreshToken), client.ClientID, tokens, refreshPolicy, now)
	if err != nil {
		if errors.Is(err, db.ErrOAuthTokenReuse) {
			log.Printf("oauth refresh token reuse revoked grant %s", grant.ID)
			if alertErr := s.approver.OAuthConnectionRevoked(context.WithoutCancel(r.Context()), grant.UserID, grant.ClientName, "старый refresh-токен использован повторно"); alertErr != nil {
				log.Printf("oauth reuse alert failed: %v", alertErr)
			}
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token was already used; the connection was revoked")
			return
		}
		if errors.Is(err, db.ErrOAuthInvalidGrant) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid, expired, or revoked")
			return
		}
		log.Printf("oauth refresh failed: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not refresh tokens")
		return
	}
	writeTokenResponse(w, access, refresh)
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "revocation request must be form-encoded")
		return
	}
	client, ok := s.authenticateClient(w, r)
	if !ok {
		return
	}
	if token := r.PostForm.Get("token"); token != "" {
		if err := s.store.RevokeOAuthToken(r.Context(), hashSecret(token), client.ClientID, s.now()); err != nil {
			log.Printf("oauth revoke failed: %v", err)
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not revoke token")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// authenticateClient applies RFC 6749 section 2.3 client authentication.
func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request) (db.OAuthClient, bool) {
	clientID := r.PostForm.Get("client_id")
	secret := r.PostForm.Get("client_secret")
	usedBasic := false
	if basicID, basicSecret, ok := r.BasicAuth(); ok {
		decodedID, errID := url.QueryUnescape(basicID)
		decodedSecret, errSecret := url.QueryUnescape(basicSecret)
		if errID != nil || errSecret != nil || (clientID != "" && clientID != decodedID) {
			writeClientError(w)
			return db.OAuthClient{}, false
		}
		clientID, secret, usedBasic = decodedID, decodedSecret, true
	}
	client, ok, err := s.store.GetOAuthClient(r.Context(), clientID)
	if err != nil {
		log.Printf("oauth client lookup failed: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not verify client")
		return db.OAuthClient{}, false
	}
	if clientID == "" || !ok {
		writeClientError(w)
		return db.OAuthClient{}, false
	}
	switch client.AuthMethod {
	case authMethodNone:
		return client, true
	case authMethodBasic, authMethodPost:
		if (client.AuthMethod == authMethodBasic) != usedBasic || !secretMatches(secret, client.SecretHash) {
			writeClientError(w)
			return db.OAuthClient{}, false
		}
		return client, true
	default:
		writeClientError(w)
		return db.OAuthClient{}, false
	}
}

func (s *Server) allowedRedirectURI(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Fragment != "" || parsed.User != nil || parsed.Host == "" {
		return false
	}
	switch parsed.Scheme {
	case "http":
		return isLoopbackHost(parsed.Hostname())
	case "https":
		return slices.Contains(trustedRedirectURIs, raw) || slices.Contains(s.extraURIs, raw)
	default:
		return false
	}
}

func (s *Server) validResource(values []string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		value = strings.TrimRight(value, "/")
		if value != s.resource && value != s.issuer {
			return false
		}
	}
	return true
}

func (s *Server) successRedirect(redirectURI, state, code string) string {
	return appendQuery(redirectURI, state, url.Values{"code": {code}, "iss": {s.issuer}})
}

func (s *Server) errorRedirect(redirectURI, state, code, description string) string {
	return appendQuery(redirectURI, state, url.Values{"error": {code}, "error_description": {description}, "iss": {s.issuer}})
}

// registeredRedirectURI resolves the redirect_uri of an authorization request.
// Loopback URIs match regardless of port (RFC 8252 section 7.3).
func registeredRedirectURI(client db.OAuthClient, requested string) (string, bool) {
	if requested == "" {
		if len(client.RedirectURIs) == 1 {
			return client.RedirectURIs[0], true
		}
		return "", false
	}
	requestedURL, err := url.Parse(requested)
	if err != nil {
		return "", false
	}
	for _, registered := range client.RedirectURIs {
		if registered == requested {
			return requested, true
		}
		registeredURL, err := url.Parse(registered)
		if err != nil || registeredURL.Scheme != "http" || requestedURL.Scheme != "http" || !isLoopbackHost(registeredURL.Hostname()) {
			continue
		}
		if registeredURL.Hostname() == requestedURL.Hostname() && registeredURL.Path == requestedURL.Path && registeredURL.RawQuery == requestedURL.RawQuery {
			return requested, true
		}
	}
	return "", false
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func appendQuery(rawURL, state string, values url.Values) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	query := parsed.Query()
	for key, entries := range values {
		query[key] = entries
	}
	if state != "" {
		query.Set("state", state)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func newTokenPair(now time.Time) (string, string, []db.OAuthToken) {
	access := accessTokenPrefix + randomToken(32)
	refresh := refreshTokenPrefix + randomToken(32)
	return access, refresh, []db.OAuthToken{
		{Hash: hashSecret(access), Kind: "access", ExpiresAt: now.Add(accessTokenTTL)},
		{Hash: hashSecret(refresh), Kind: "refresh", ExpiresAt: now.Add(refreshTokenTTL)},
	}
}

func writeTokenResponse(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         Scope,
	})
}

func pkceMatches(verifier, challenge string) bool {
	if !validPKCEValue(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

// validPKCEValue checks RFC 7636 length and alphabet for verifiers and S256
// challenges (a challenge is always 43 characters).
func validPKCEValue(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' || r == '~') {
			return false
		}
	}
	return true
}

func cleanClientName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	return truncateRunes(strings.Join(strings.Fields(name), " "), maxClientNameLen)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

// clientIP identifies the browser for rate limits and the approval prompt.
// Fly's proxy overwrites Fly-Client-IP, so it cannot be spoofed there.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("Fly-Client-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) allowRequest(kind string, r *http.Request) bool {
	return s.limiter.allow(kind, globalPerHour, time.Hour) && s.limiter.allow(kind+":"+clientIP(r), perIPPerHour, time.Hour)
}

func displayClientName(name string) string {
	if name == "" {
		return "MCP-клиент"
	}
	return name
}

func randomInt(limit int) int {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return int(value.Int64())
}

func randomToken(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

func hashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func secretMatches(secret, hash string) bool {
	if secret == "" || hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hash)) == 1
}

func allowMetadataMethod(w http.ResponseWriter, r *http.Request) bool {
	setCORS(w)
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return false
	case http.MethodGet, http.MethodHead:
		return true
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func writeClientError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="telegram-bridge-oauth"`)
	writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
}

type limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	windows map[string]limiterWindow
}

type limiterWindow struct {
	start time.Time
	count int
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, windows: map[string]limiterWindow{}}
}

// allow is a fixed-window counter per key.
func (l *limiter) allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.windows) > maxLimiterKeys {
		for existing, entry := range l.windows {
			if now.Sub(entry.start) >= window {
				delete(l.windows, existing)
			}
		}
	}
	current := l.windows[key]
	if now.Sub(current.start) >= window {
		current = limiterWindow{start: now}
	}
	if current.count >= limit {
		return false
	}
	current.count++
	l.windows[key] = current
	return true
}
