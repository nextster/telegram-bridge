package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/mcpserver"
	"github.com/nextster/telegram-bridge/internal/oauth"
)

const (
	testRedirect = "http://127.0.0.1:43123/callback"
	adminUserID  = 4242
)

type fakeApprover struct {
	mu       sync.Mutex
	requests []string
	revoked  []string
}

func (f *fakeApprover) OAuthApprovalLink(_ context.Context, requestID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, requestID)
	return "https://t.me/bridge_test_bot?start=oauth_" + requestID, nil
}

func (f *fakeApprover) OAuthConnectionRevoked(_ context.Context, clientName, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, clientName)
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
	h.oauth, err = oauth.New(h.server.URL, store, h.approver, oauth.Options{Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.oauth.Register(mux)
	mux.Handle("/mcp", mcpserver.New(nil, "static-secret", mcpserver.Options{
		VerifyToken:         h.oauth.VerifyAccessToken,
		ResourceMetadataURL: h.oauth.ResourceMetadataURL(),
	}))
	handler = mux
	h.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

var pageSecrets = regexp.MustCompile(`JSON\.stringify\(\{request: "([^"]+)", secret: "([^"]+)"\}\)`)
var pageCode = regexp.MustCompile(`<div class="code"[^>]*>(\d+)</div>`)
var pageLink = regexp.MustCompile(`href="https://t\.me/bridge_test_bot\?start=oauth_([^"]+)"`)

type pageRequest struct {
	id, secret, code string
}

// openPage loads the authorization page like a browser.
func (h *harness) openPage(authURL string) pageRequest {
	h.t.Helper()
	response, err := h.client.Get(authURL)
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		h.t.Fatalf("authorize status=%d body=%s", response.StatusCode, body)
	}
	secrets := pageSecrets.FindStringSubmatch(string(body))
	code := pageCode.FindStringSubmatch(string(body))
	link := pageLink.FindStringSubmatch(string(body))
	if secrets == nil || code == nil || link == nil || link[1] != secrets[1] {
		h.t.Fatalf("authorization page is missing request data: %s", body)
	}
	return pageRequest{id: secrets[1], secret: secrets[2], code: code[1]}
}

// approve opens the authorization page, lets the bot decide with the picked
// number, and returns the redirect the browser page would follow.
func (h *harness) approve(authURL string, pick func(correct string) string) string {
	h.t.Helper()
	page := h.openPage(authURL)
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
			grants, err := h.store.ListOAuthGrants(context.Background())
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

	page := h.openPage(h.authorizeURL(clientID, strings.Repeat("v", 64)))
	if _, changed, err := h.store.DecideOAuthRequest(context.Background(), page.id, true, adminUserID, h.clock.Now()); err != nil || !changed {
		t.Fatalf("decide changed=%t err=%v", changed, err)
	}
	if status := h.status(page.id, page.secret); status["status"] != "approved" || !strings.Contains(status["redirect"], "code=") {
		t.Fatalf("first status = %v", status)
	}
	if status := h.status(page.id, page.secret); status["status"] != "done" || status["redirect"] != "" {
		t.Fatalf("second status minted another code: %v", status)
	}

	late := h.openPage(h.authorizeURL(clientID, strings.Repeat("v", 64)))
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
