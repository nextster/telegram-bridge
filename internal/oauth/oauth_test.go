package oauth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/mcpserver"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/oauth"
)

const (
	testRedirect    = "http://127.0.0.1:43123/callback"
	adminUserID     = 4242
	testBotID       = "123456"
	testLoginSecret = "login-secret"
)

type fakeApprover struct {
	mu       sync.Mutex
	requests []string
	revoked  []string
	created  []int64
	prompted []string
}

func (f *fakeApprover) OAuthApprovalRequested(_ context.Context, userID int64, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompted = append(f.prompted, fmt.Sprintf("%d:%s", userID, requestID))
	return nil
}

// fakeTelegramLogin is the token endpoint of Telegram Login. Codes are handed
// out by the harness in place of the user's consent on oauth.telegram.org.
type fakeTelegramLogin struct {
	mu     sync.Mutex
	key    *rsa.PrivateKey
	codes  map[string]fakeLoginCode
	mutate func(claims map[string]any)
}

type fakeLoginCode struct {
	userID                        int64
	nonce, challenge, redirectURI string
}

func (f *fakeTelegramLogin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || user != testBotID || password != testLoginSecret || r.ParseForm() != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	entry, found := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	mutate := f.mutate
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || entry.redirectURI != r.PostForm.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != entry.challenge {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": "https://oauth.telegram.org", "aud": testBotID, "sub": "1234123412341234123",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": entry.nonce, "id": entry.userID,
	}
	if mutate != nil {
		mutate(claims)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, nil)
	if err != nil {
		panic(err)
	}
	payload, _ := json.Marshal(claims)
	object, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	token, _ := object.CompactSerialize()
	_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "telegram-access", "token_type": "Bearer", "id_token": token})
}

func (f *fakeApprover) OAuthApprovalLink(_ context.Context, requestID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, requestID)
	return "https://t.me/bridge_test_bot?start=oauth_" + requestID, nil
}

func (f *fakeApprover) OAuthConnectionRevoked(_ context.Context, _ int64, clientName, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, clientName)
	return nil
}

func (f *fakeApprover) OAuthConnectionCreated(_ context.Context, userID int64, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, userID)
	return nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t        *testing.T
	store    *db.Store
	approver *fakeApprover
	login    *fakeTelegramLogin
	clock    *clock
	server   *httptest.Server
	oauth    *oauth.Server
	client   *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/oauth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &harness{t: t, store: store, approver: &fakeApprover{}, clock: &clock{now: time.Now()}}
	var handler http.Handler
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(h.server.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h.login = &fakeTelegramLogin{key: key, codes: map[string]fakeLoginCode{}}
	loginServer := httptest.NewServer(h.login)
	t.Cleanup(loginServer.Close)
	h.oauth, err = oauth.New(h.server.URL, store, h.approver, oauth.Options{Now: h.clock.Now, TelegramLogin: oauth.TelegramLogin{
		ClientID:     testBotID,
		ClientSecret: testLoginSecret,
		TokenURL:     loginServer.URL,
		KeySet:       &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.oauth.Register(mux)
	mux.Handle("/mcp", mcpserver.New(mcpserver.Options{
		VerifyToken: func(ctx context.Context, token string) (int64, time.Time, error) {
			if token == "static-secret" {
				return adminUserID, time.Now().Add(time.Hour), nil
			}
			userID, expires, ok, err := h.oauth.VerifyAccessToken(ctx, token)
			if err != nil {
				return 0, time.Time{}, err
			}
			if !ok {
				return 0, time.Time{}, auth.ErrInvalidToken
			}
			return userID, expires, nil
		},
		Accounts: func(userID int64) (mcpserver.Monitor, error) {
			if userID != adminUserID {
				return nil, fmt.Errorf("user %d is not connected", userID)
			}
			return emptyAccount{}, nil
		},
		ResourceMetadataURL: h.oauth.ResourceMetadataURL(),
	}))
	handler = mux
	h.client = newBrowser()
	return h
}

// newBrowser is a browser with its own cookies that does not follow redirects.
func newBrowser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var pageSecrets = regexp.MustCompile(`JSON\.stringify\(\{request: "([^"]+)", secret: "([^"]+)"\}\)`)
var pageSignIn = regexp.MustCompile(`href="/oauth/telegram/start\?request=([^"]+)"`)
var pageCode = regexp.MustCompile(`<div class="code"[^>]*>(\d+)</div>`)
var pageLink = regexp.MustCompile(`href="https://t\.me/bridge_test_bot\?start=oauth_([^"]+)"`)

type pageRequest struct {
	id, secret, code string
}

// openPage loads the authorization page like a browser and returns the
// request ID from its sign-in link.
func (h *harness) openPage(authURL string) string {
	h.t.Helper()
	response, err := h.client.Get(authURL)
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	signIn := pageSignIn.FindStringSubmatch(string(body))
	if response.StatusCode != http.StatusOK || signIn == nil || strings.Contains(string(body), "t.me/") {
		h.t.Fatalf("authorize status=%d body=%s", response.StatusCode, body)
	}
	return signIn[1]
}

// browse sends a GET from browser and returns the status, Location, and body.
func (h *harness) browse(browser *http.Client, target string) (int, string, string) {
	h.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = h.server.URL + target
	}
	response, err := browser.Get(target)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, response.Header.Get("Location"), string(body)
}

// startSignIn follows the sign-in link to Telegram and returns the callback
// URL Telegram would redirect to after userID consents.
func (h *harness) startSignIn(browser *http.Client, requestID string, userID int64) (string, int, string) {
	h.t.Helper()
	status, location, body := h.browse(browser, "/oauth/telegram/start?request="+requestID)
	if status != http.StatusFound {
		return "", status, body
	}
	telegram, err := url.Parse(location)
	if err != nil {
		h.t.Fatal(err)
	}
	query := telegram.Query()
	if query.Get("client_id") != testBotID || query.Get("scope") != "openid profile" || query.Get("code_challenge_method") != "S256" {
		h.t.Fatalf("telegram authorization URL = %s", location)
	}
	code := "code-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h.login.mu.Lock()
	h.login.codes[code] = fakeLoginCode{userID: userID, nonce: query.Get("nonce"), challenge: query.Get("code_challenge"), redirectURI: query.Get("redirect_uri")}
	h.login.mu.Unlock()
	return query.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {query.Get("state")}}.Encode(), status, body
}

// openApproval opens the authorization page, signs in with Telegram as
// userID, and returns the approval step.
func (h *harness) openApproval(authURL string, userID int64) pageRequest {
	h.t.Helper()
	requestID := h.openPage(authURL)
	callback, status, body := h.startSignIn(h.client, requestID, userID)
	if callback == "" {
		h.t.Fatalf("sign-in start status=%d body=%s", status, body)
	}
	status, location, body := h.browse(h.client, callback)
	if status != http.StatusSeeOther || !strings.HasPrefix(location, "/oauth/authorize/continue?") {
		h.t.Fatalf("callback status=%d location=%q body=%s", status, location, body)
	}
	status, _, body = h.browse(h.client, location)
	if status != http.StatusOK {
		h.t.Fatalf("continue status=%d body=%s", status, body)
	}
	secrets := pageSecrets.FindStringSubmatch(body)
	code := pageCode.FindStringSubmatch(body)
	link := pageLink.FindStringSubmatch(body)
	if secrets == nil || code == nil || link == nil || link[1] != secrets[1] || secrets[1] != requestID {
		h.t.Fatalf("authorization page is missing request data: %s", body)
	}
	return pageRequest{id: secrets[1], secret: secrets[2], code: code[1]}
}

// approve opens the authorization page, lets the bot decide with the picked
// number, and returns the redirect the browser page would follow.
func (h *harness) approve(authURL string, pick func(correct string) string) string {
	h.t.Helper()
	page := h.openApproval(authURL, adminUserID)
	stored, ok, err := h.store.GetOAuthRequest(context.Background(), page.id)
	if err != nil || !ok || stored.ApprovalCode != page.code {
		h.t.Fatalf("stored request does not match page: %+v err=%v", stored, err)
	}
	if status := h.status(page.id, page.secret); status["status"] != "pending" {
		h.t.Fatalf("status before decision = %v", status)
	}
	choice := pick(page.code)
	if _, changed, err := h.store.DecideOAuthRequest(context.Background(), page.id, choice == page.code, adminUserID, h.clock.Now()); err != nil || !changed {
		h.t.Fatalf("decide changed=%t err=%v", changed, err)
	}
	if status := h.status(page.id, "wrong-secret"); status["status"] != "expired" {
		h.t.Fatalf("status with a foreign browser secret = %v", status)
	}
	return h.status(page.id, page.secret)["redirect"]
}

func (h *harness) status(requestID, secret string) map[string]string {
	h.t.Helper()
	payload, _ := json.Marshal(map[string]string{"request": requestID, "secret": secret})
	response, err := h.client.Post(h.server.URL+"/oauth/authorize/status", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]string
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) register(metadata map[string]any) (int, map[string]any) {
	h.t.Helper()
	payload, _ := json.Marshal(metadata)
	response, err := h.client.Post(h.server.URL+"/oauth/register", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	_ = json.NewDecoder(response.Body).Decode(&result)
	return response.StatusCode, result
}

func (h *harness) tokenRequest(form url.Values) (int, map[string]any) {
	h.t.Helper()
	response, err := h.client.PostForm(h.server.URL+"/oauth/token", form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	_ = json.NewDecoder(response.Body).Decode(&result)
	return response.StatusCode, result
}

func (h *harness) mcpStatus(token string) (int, string) {
	h.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode, response.Header.Get("WWW-Authenticate")
}

func pickCorrect(correct string) string { return correct }

type emptyAccount struct{}

func (emptyAccount) ListDialogs(context.Context, string, int) ([]monitor.TelegramDialog, error) {
	return nil, nil
}
func (emptyAccount) SearchMessages(context.Context, monitor.MessageSearchOptions) ([]monitor.TelegramMessage, error) {
	return nil, nil
}
func (emptyAccount) GetHistoryPage(context.Context, monitor.HistoryOptions) (monitor.HistoryPage, error) {
	return monitor.HistoryPage{}, nil
}

func TestSDKClientConnectsAfterTelegramApproval(t *testing.T) {
	for _, method := range []string{"", "none", "client_secret_post"} {
		t.Run("auth_method="+method, func(t *testing.T) {
			h := newHarness(t)
			handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
				DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
					ClientName:              "Claude Code (telegram-bridge)",
					RedirectURIs:            []string{testRedirect},
					TokenEndpointAuthMethod: method,
				}},
				RedirectURL: testRedirect,
				AuthorizationCodeFetcher: func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
					redirect, err := url.Parse(h.approve(args.URL, pickCorrect))
					if err != nil {
						return nil, err
					}
					if redirect.Query().Get("iss") != h.server.URL {
						return nil, fmt.Errorf("redirect iss = %q", redirect.Query().Get("iss"))
					}
					return &auth.AuthorizationResult{Code: redirect.Query().Get("code"), State: redirect.Query().Get("state")}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "oauth-test", Version: "1.0.0"}, nil)
			session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint:             h.server.URL + "/mcp",
				OAuthHandler:         handler,
				DisableStandaloneSSE: true,
			}, nil)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer session.Close()
			tools, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			if len(tools.Tools) != 3 {
				t.Fatalf("tool count = %d", len(tools.Tools))
			}
			grants, err := h.store.ListOAuthGrants(context.Background(), adminUserID)
			if err != nil || len(grants) != 1 || grants[0].ClientName != "Claude Code (telegram-bridge)" || grants[0].UserID != adminUserID {
				t.Fatalf("grants = %+v err=%v", grants, err)
			}
		})
	}
}

func TestStaticTokenStillWorksAndChallengeAdvertisesMetadata(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.mcpStatus("static-secret"); status != http.StatusOK {
		t.Fatalf("static token status = %d", status)
	}
	status, challenge := h.mcpStatus("tba_forged")
	if status != http.StatusUnauthorized || !strings.Contains(challenge, `resource_metadata="`+h.server.URL+`/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("status=%d challenge=%q", status, challenge)
	}
}

func TestMetadataDocuments(t *testing.T) {
	h := newHarness(t)
	var resource map[string]any
	getJSON(t, h.server.URL+"/.well-known/oauth-protected-resource/mcp", &resource)
	if resource["resource"] != h.server.URL+"/mcp" || fmt.Sprint(resource["authorization_servers"]) != "["+h.server.URL+"]" {
		t.Fatalf("resource metadata = %v", resource)
	}
	var server map[string]any
	getJSON(t, h.server.URL+"/.well-known/oauth-authorization-server", &server)
	if server["issuer"] != h.server.URL || fmt.Sprint(server["code_challenge_methods_supported"]) != "[S256]" || server["registration_endpoint"] != h.server.URL+"/oauth/register" {
		t.Fatalf("authorization server metadata = %v", server)
	}
}

func TestRegistrationAcceptsOnlyLoopbackAndTrustedCallbacks(t *testing.T) {
	h := newHarness(t)
	cases := map[string]int{
		"http://localhost:5555/callback":          http.StatusCreated,
		"http://[::1]:5555/callback":              http.StatusCreated,
		"https://claude.ai/api/mcp/auth_callback": http.StatusCreated,
		"https://evil.example/callback":           http.StatusBadRequest,
		"http://evil.example/callback":            http.StatusBadRequest,
		"cursor://anysphere.cursor-mcp/oauth":     http.StatusBadRequest,
		"http://127.0.0.1:5555/callback#fragment": http.StatusBadRequest,
	}
	for uri, want := range cases {
		status, body := h.register(map[string]any{"redirect_uris": []string{uri}, "token_endpoint_auth_method": "none"})
		if status != want {
			t.Errorf("register %s status=%d want=%d body=%v", uri, status, want, body)
		}
	}
}

func TestAuthorizationCodeIsSingleUseAndBoundToPKCE(t *testing.T) {
	h := newHarness(t)
	clientID, verifier, redirect := h.authorizeManually(pickCorrect)
	code := redirect.Query().Get("code")
	if redirect.Query().Get("state") != "state-1" || code == "" {
		t.Fatalf("redirect = %s", redirect)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "redirect_uri": {testRedirect}}

	form.Set("code_verifier", strings.Repeat("x", 43))
	if status, body := h.tokenRequest(form); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier status=%d body=%v", status, body)
	}
	form.Set("code_verifier", verifier)
	status, body := h.tokenRequest(form)
	if status != http.StatusOK || body["token_type"] != "Bearer" {
		t.Fatalf("exchange status=%d body=%v", status, body)
	}
	if status, _ := h.mcpStatus(body["access_token"].(string)); status != http.StatusOK {
		t.Fatalf("access token status = %d", status)
	}
	if status, body := h.tokenRequest(form); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reused code status=%d body=%v", status, body)
	}
	h.approver.mu.Lock()
	defer h.approver.mu.Unlock()
	if len(h.approver.created) != 1 || h.approver.created[0] != adminUserID {
		t.Fatalf("connection alerts = %v", h.approver.created)
	}
}

func TestLogoutCancelsApprovedButUnexchangedCode(t *testing.T) {
	h := newHarness(t)
	clientID, verifier, redirect := h.authorizeManually(pickCorrect)
	if err := h.store.RevokeOAuthGrantsForUser(context.Background(), adminUserID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {redirect.Query().Get("code")}, "redirect_uri": {testRedirect}, "code_verifier": {verifier}}
	if status, body := h.tokenRequest(form); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("exchange after logout status=%d body=%v", status, body)
	}
	if grants, err := h.store.ListOAuthGrants(context.Background(), adminUserID); err != nil || len(grants) != 0 {
		t.Fatalf("grants after logout = %v err=%v", grants, err)
	}
}

func TestWrongNumberOrDenyRedirectsWithAccessDenied(t *testing.T) {
	h := newHarness(t)
	wrong := func(correct string) string { return correct + "0" }
	_, _, redirect := h.authorizeManually(wrong)
	if redirect.Query().Get("error") != "access_denied" || redirect.Query().Get("code") != "" || redirect.Query().Get("state") != "state-1" {
		t.Fatalf("redirect = %s", redirect)
	}
}

func TestRefreshRotatesTokensAndRevokeDisconnects(t *testing.T) {
	h := newHarness(t)
	clientID, verifier, redirect := h.authorizeManually(pickCorrect)
	_, first := h.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}})
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {first["refresh_token"].(string)}}
	status, second := h.tokenRequest(refreshForm)
	if status != http.StatusOK || second["refresh_token"] == first["refresh_token"] {
		t.Fatalf("refresh status=%d body=%v", status, second)
	}
	if status, body := h.tokenRequest(refreshForm); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reused refresh token status=%d body=%v", status, body)
	}
	otherClient := url.Values{"grant_type": {"refresh_token"}, "client_id": {"tbc_other"}, "refresh_token": {second["refresh_token"].(string)}}
	if status, _ := h.tokenRequest(otherClient); status != http.StatusUnauthorized {
		t.Fatalf("foreign client refresh status = %d", status)
	}
	if status, _ := h.mcpStatus(second["access_token"].(string)); status != http.StatusOK {
		t.Fatalf("rotated access token status = %d", status)
	}
	response, err := h.client.PostForm(h.server.URL+"/oauth/revoke", url.Values{"client_id": {clientID}, "token": {second["refresh_token"].(string)}})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%v err=%v", response, err)
	}
	response.Body.Close()
	for _, token := range []string{first["access_token"].(string), second["access_token"].(string)} {
		if status, _ := h.mcpStatus(token); status != http.StatusUnauthorized {
			t.Fatalf("revoked access token status = %d", status)
		}
	}
}

func TestAuthorizeRejectsMismatchedRedirectAndMissingPKCE(t *testing.T) {
	h := newHarness(t)
	_, registered := h.register(map[string]any{"redirect_uris": []string{testRedirect}, "token_endpoint_auth_method": "none"})
	clientID := registered["client_id"].(string)
	query := url.Values{"response_type": {"code"}, "client_id": {clientID}, "state": {"s"}}

	query.Set("redirect_uri", "http://127.0.0.1:1/other")
	if response := h.get("/oauth/authorize?" + query.Encode()); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched redirect status = %d", response.StatusCode)
	}
	query.Set("redirect_uri", "http://127.0.0.1:9999/callback")
	response := h.get("/oauth/authorize?" + query.Encode())
	if response.StatusCode != http.StatusBadRequest || response.Header.Get("Location") != "" {
		t.Fatalf("missing PKCE must not redirect: status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
	if len(h.approver.requests) != 0 {
		t.Fatal("an invalid request produced an approval link")
	}
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	response, err := h.client.Get(h.server.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	response.Body.Close()
	return response
}

func (h *harness) authorizeManually(pick func(string) string) (string, string, *url.URL) {
	h.t.Helper()
	status, registered := h.register(map[string]any{
		"client_name": "Codex", "redirect_uris": []string{testRedirect}, "token_endpoint_auth_method": "none",
	})
	if status != http.StatusCreated {
		h.t.Fatalf("register status=%d body=%v", status, registered)
	}
	clientID := registered["client_id"].(string)
	verifier := strings.Repeat("v", 64)
	redirect, err := url.Parse(h.approve(h.authorizeURL(clientID, verifier), pick))
	if err != nil {
		h.t.Fatal(err)
	}
	return clientID, verifier, redirect
}

func (h *harness) authorizeURL(clientID, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirect},
		"state":                 {"state-1"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {h.server.URL + "/mcp"},
	}
	return h.server.URL + "/oauth/authorize?" + query.Encode()
}

func (h *harness) registerPublicClient() string {
	h.t.Helper()
	status, registered := h.register(map[string]any{"client_name": "Codex", "redirect_uris": []string{testRedirect}, "token_endpoint_auth_method": "none"})
	if status != http.StatusCreated {
		h.t.Fatalf("register status=%d body=%v", status, registered)
	}
	return registered["client_id"].(string)
}

func getJSON(t *testing.T, target string, value any) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d", target, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(value); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeRejectsHEADAndLimitsRequestsPerIP(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerPublicClient()
	request, _ := http.NewRequest(http.MethodHead, h.authorizeURL(clientID, strings.Repeat("v", 64)), nil)
	response, err := h.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status = %d", response.StatusCode)
	}
	for i := 0; i < 20; i++ {
		h.openPage(h.authorizeURL(clientID, strings.Repeat("v", 64)))
	}
	if response := h.get(strings.TrimPrefix(h.authorizeURL(clientID, strings.Repeat("v", 64)), h.server.URL)); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("21st authorize from one IP status = %d", response.StatusCode)
	}
	h.clock.Advance(time.Hour)
	h.openPage(h.authorizeURL(clientID, strings.Repeat("v", 64)))
}

func TestApprovedRequestMintsOneCodeOnlyBeforeExpiry(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerPublicClient()

	page := h.openApproval(h.authorizeURL(clientID, strings.Repeat("v", 64)), adminUserID)
	if _, changed, err := h.store.DecideOAuthRequest(context.Background(), page.id, true, adminUserID, h.clock.Now()); err != nil || !changed {
		t.Fatalf("decide changed=%t err=%v", changed, err)
	}
	if status := h.status(page.id, page.secret); status["status"] != "approved" || !strings.Contains(status["redirect"], "code=") {
		t.Fatalf("first status = %v", status)
	}
	if status := h.status(page.id, page.secret); status["status"] != "done" || status["redirect"] != "" {
		t.Fatalf("second status minted another code: %v", status)
	}

	late := h.openApproval(h.authorizeURL(clientID, strings.Repeat("v", 64)), adminUserID)
	if _, changed, err := h.store.DecideOAuthRequest(context.Background(), late.id, true, adminUserID, h.clock.Now()); err != nil || !changed {
		t.Fatalf("decide changed=%t err=%v", changed, err)
	}
	h.clock.Advance(11 * time.Minute)
	if status := h.status(late.id, late.secret); status["status"] != "expired" {
		t.Fatalf("expired approved request status = %v", status)
	}
}

func TestReusedRefreshTokenRevokesConnectionAfterGrace(t *testing.T) {
	h := newHarness(t)
	clientID, verifier, redirect := h.authorizeManually(pickCorrect)
	_, first := h.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}})
	oldRefresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {first["refresh_token"].(string)}}
	status, second := h.tokenRequest(oldRefresh)
	if status != http.StatusOK {
		t.Fatalf("refresh status=%d body=%v", status, second)
	}

	if status, body := h.tokenRequest(oldRefresh); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("retry within grace status=%d body=%v", status, body)
	}
	if status, _ := h.mcpStatus(second["access_token"].(string)); status != http.StatusOK || len(h.approver.revoked) != 0 {
		t.Fatalf("retry within grace revoked the connection: status=%d revoked=%v", status, h.approver.revoked)
	}

	h.clock.Advance(time.Minute)
	if status, body := h.tokenRequest(oldRefresh); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reuse status=%d body=%v", status, body)
	}
	if status, _ := h.mcpStatus(second["access_token"].(string)); status != http.StatusUnauthorized {
		t.Fatalf("access token after refresh reuse status = %d", status)
	}
	newRefresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {second["refresh_token"].(string)}}
	if status, _ := h.tokenRequest(newRefresh); status != http.StatusBadRequest {
		t.Fatalf("rotated refresh token after reuse status = %d", status)
	}
	if len(h.approver.revoked) != 1 || h.approver.revoked[0] != "Codex" {
		t.Fatalf("owner alerts = %v", h.approver.revoked)
	}
}

func TestSignInBindsTheRequestToTheBrowserThatOpenedIt(t *testing.T) {
	h := newHarness(t)
	clientID := h.registerPublicClient()
	verifier := strings.Repeat("v", 64)
	requestID := h.openPage(h.authorizeURL(clientID, verifier))

	// A link forwarded to another browser cannot sign in or finish a sign-in.
	stranger := newBrowser()
	if status, _, body := h.browse(stranger, "/oauth/telegram/start?request="+requestID); status != http.StatusBadRequest || !strings.Contains(body, "не в том браузере") {
		t.Fatalf("foreign browser start status=%d body=%s", status, body)
	}
	serverURL, _ := url.Parse(h.server.URL + "/oauth/")
	forged := newBrowser()
	forged.Jar.SetCookies(serverURL, []*http.Cookie{{Name: "tb_oauth_" + requestID, Value: "forged", Path: "/oauth/"}})
	if status, _, _ := h.browse(forged, "/oauth/telegram/start?request="+requestID); status != http.StatusBadRequest {
		t.Fatalf("forged browser cookie start status = %d", status)
	}
	callback, _, _ := h.startSignIn(h.client, requestID, 777)
	for _, browser := range []*http.Client{stranger, forged} {
		if status, _, _ := h.browse(browser, callback); status != http.StatusBadRequest {
			t.Fatalf("foreign browser callback status = %d", status)
		}
	}
	if request, _, _ := h.store.GetOAuthRequest(context.Background(), requestID); request.BoundUserID != 0 {
		t.Fatalf("request bound through another browser: %+v", request)
	}

	// The owner of the browser signs in as 777; only 777 is asked and can decide.
	status, location, _ := h.browse(h.client, callback)
	if status != http.StatusSeeOther {
		t.Fatalf("callback status = %d", status)
	}
	if status, _, _ := h.browse(h.client, callback); status != http.StatusBadRequest {
		t.Fatalf("replayed callback status = %d", status)
	}
	h.approver.mu.Lock()
	prompted := append([]string(nil), h.approver.prompted...)
	h.approver.mu.Unlock()
	if len(prompted) != 1 || prompted[0] != "777:"+requestID {
		t.Fatalf("prompts = %v", prompted)
	}
	_, _, body := h.browse(h.client, location)
	secrets := pageSecrets.FindStringSubmatch(body)
	if secrets == nil {
		t.Fatalf("continue page = %s", body)
	}
	if status, _, _ := h.browse(stranger, location); status != http.StatusBadRequest {
		t.Fatalf("foreign browser continue status = %d", status)
	}
	if _, changed, err := h.store.DecideOAuthRequest(context.Background(), requestID, true, adminUserID, h.clock.Now()); err != nil || changed {
		t.Fatalf("another user decided the request: changed=%t err=%v", changed, err)
	}
	if _, changed, err := h.store.DecideOAuthRequest(context.Background(), requestID, true, 777, h.clock.Now()); err != nil || !changed {
		t.Fatalf("signed-in user could not decide: changed=%t err=%v", changed, err)
	}
	redirect, err := url.Parse(h.status(requestID, secrets[2])["redirect"])
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {redirect.Query().Get("code")}, "redirect_uri": {testRedirect}, "code_verifier": {verifier}}
	if status, body := h.tokenRequest(form); status != http.StatusOK {
		t.Fatalf("exchange status=%d body=%v", status, body)
	}
	if grants, err := h.store.ListOAuthGrants(context.Background(), 777); err != nil || len(grants) != 1 {
		t.Fatalf("grants for the signed-in user = %v err=%v", grants, err)
	}
	if grants, _ := h.store.ListOAuthGrants(context.Background(), adminUserID); len(grants) != 0 {
		t.Fatalf("grant went to another user: %v", grants)
	}
}

func TestSignInRejectsInvalidIDTokens(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"wrong nonce":    func(claims map[string]any) { claims["nonce"] = "other" },
		"wrong audience": func(claims map[string]any) { claims["aud"] = "999" },
		"wrong issuer":   func(claims map[string]any) { claims["iss"] = "https://attacker.example" },
		"expired":        func(claims map[string]any) { claims["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no user id":     func(claims map[string]any) { delete(claims, "id") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.login.mutate = mutate
			requestID := h.openPage(h.authorizeURL(h.registerPublicClient(), strings.Repeat("v", 64)))
			callback, _, _ := h.startSignIn(h.client, requestID, 777)
			if status, _, _ := h.browse(h.client, callback); status != http.StatusBadGateway {
				t.Fatalf("callback status = %d", status)
			}
			if request, _, _ := h.store.GetOAuthRequest(context.Background(), requestID); request.BoundUserID != 0 {
				t.Fatalf("request bound with an invalid token: %+v", request)
			}
			if len(h.approver.prompted) != 0 {
				t.Fatalf("prompt sent for an invalid token: %v", h.approver.prompted)
			}
		})
	}
}
