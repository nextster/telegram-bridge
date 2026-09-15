package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

const webLoginTimeout = 10 * time.Minute

type webLoginPhase string

const (
	webLoginStarting   webLoginPhase = "starting"
	webLoginPrompt     webLoginPhase = "prompt"
	webLoginAuthorized webLoginPhase = "authorized"
	webLoginError      webLoginPhase = "error"
)

type webLoginManager struct {
	store    *db.Store
	accounts Accounts
	notifier UserNotifier

	mu    sync.Mutex
	flows map[string]*webLoginFlow
}

type webLoginFlow struct {
	token  string
	chatID int64
	phone  string

	input  chan string
	cancel context.CancelFunc

	mu            sync.Mutex
	phase         webLoginPhase
	promptKind    monitor.LoginPromptKind
	promptMessage string
	result        monitor.LoginResult
	errText       string
	startedAt     time.Time
	updatedAt     time.Time
}

type webLoginView struct {
	Token         string
	Phone         string
	Phase         webLoginPhase
	PromptKind    monitor.LoginPromptKind
	PromptMessage string
	InputLabel    string
	InputType     string
	InputMode     string
	Autocomplete  string
	ButtonLabel   string
	NeedsInput    bool
	AutoRefresh   bool
	CanRestart    bool
	UserID        int64
	Error         string
	PageError     string
	Notice        string
}

func newWebLoginManager(store *db.Store, accounts Accounts, notifier UserNotifier) *webLoginManager {
	if notifier == nil {
		notifier = nopUserNotifier{}
	}
	return &webLoginManager{
		store:    store,
		accounts: accounts,
		notifier: notifier,
		flows:    make(map[string]*webLoginFlow),
	}
}

// get returns a login flow that is still within its lifetime. Older flows are
// dropped so an old link no longer shows the phone number or user ID.
func (m *webLoginManager) get(token string) *webLoginFlow {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for key, flow := range m.flows {
		if now.Sub(flow.startedAt) > webLoginTimeout {
			flow.cancel()
			delete(m.flows, key)
		}
	}
	return m.flows[strings.TrimSpace(token)]
}

func (m *webLoginManager) start(record db.LoginToken) (*webLoginFlow, error) {
	return m.startRecord(record, false)
}

func (m *webLoginManager) restart(record db.LoginToken) (*webLoginFlow, error) {
	return m.startRecord(record, true)
}

func (m *webLoginManager) startRecord(record db.LoginToken, replace bool) (*webLoginFlow, error) {
	if m.accounts == nil {
		return nil, errors.New("telegram user API is not configured")
	}
	m.mu.Lock()
	if current := m.flows[record.Token]; current != nil {
		if !replace {
			m.mu.Unlock()
			return current, nil
		}
		current.cancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), webLoginTimeout)
	now := time.Now().UTC()
	flow := &webLoginFlow{
		token:         record.Token,
		chatID:        record.ChatID,
		phone:         strings.TrimSpace(record.Phone),
		input:         make(chan string, 1),
		cancel:        cancel,
		phase:         webLoginStarting,
		promptMessage: "Sending code...",
		startedAt:     now,
		updatedAt:     now,
	}
	m.flows[record.Token] = flow
	m.mu.Unlock()

	go m.run(ctx, flow)
	return flow, nil
}

func (m *webLoginManager) run(ctx context.Context, flow *webLoginFlow) {
	// The session is saved only if the Telegram account that logs in is the
	// same user who requested the link in the bot.
	result, err := m.accounts.Login(ctx, flow.chatID, monitor.LoginOptions{
		Phone: flow.phone,
		Prompt: func(ctx context.Context, req monitor.LoginPromptRequest) (string, error) {
			return flow.prompt(ctx, req)
		},
	})
	if err == nil {
		if markErr := m.store.MarkLoginTokenUsed(context.Background(), flow.token); markErr != nil {
			err = markErr
		}
	}
	flow.finish(result, err)
	if err == nil {
		m.notifyLogin(flow.chatID)
	}
}

func (f *webLoginFlow) prompt(ctx context.Context, req monitor.LoginPromptRequest) (string, error) {
	f.setPrompt(req.Kind, req.Message)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case value := <-f.input:
			value = strings.TrimSpace(value)
			if value == "" {
				f.setPrompt(req.Kind, "Empty value. Try again.")
				continue
			}
			f.setChecking()
			return value, nil
		}
	}
}

func (f *webLoginFlow) submit(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty value")
	}

	f.mu.Lock()
	phase := f.phase
	f.mu.Unlock()
	if phase != webLoginPrompt {
		return errors.New("login is not waiting for input")
	}

	select {
	case f.input <- value:
		f.setChecking()
		return nil
	default:
		return errors.New("login is already checking a value")
	}
}

func (f *webLoginFlow) setPrompt(kind monitor.LoginPromptKind, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase = webLoginPrompt
	f.promptKind = kind
	f.promptMessage = strings.TrimSpace(message)
	f.errText = ""
	f.updatedAt = time.Now().UTC()
}

func (f *webLoginFlow) setChecking() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase = webLoginStarting
	f.promptKind = ""
	f.promptMessage = "Checking..."
	f.updatedAt = time.Now().UTC()
}

func (f *webLoginFlow) finish(result monitor.LoginResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = result
	f.updatedAt = time.Now().UTC()
	if err != nil {
		f.phase = webLoginError
		f.promptKind = ""
		f.errText = webLoginErrorMessage(err)
		return
	}
	f.phase = webLoginAuthorized
	f.promptKind = ""
	if result.AlreadyAuthorized {
		f.promptMessage = "Already authorized."
	} else {
		f.promptMessage = "Authorized."
	}
}

func (f *webLoginFlow) snapshot() webLoginView {
	f.mu.Lock()
	defer f.mu.Unlock()

	view := webLoginView{
		Token:         f.token,
		Phone:         f.phone,
		Phase:         f.phase,
		PromptKind:    f.promptKind,
		PromptMessage: f.promptMessage,
		UserID:        f.result.UserID,
		Error:         f.errText,
		AutoRefresh:   f.phase == webLoginStarting,
		CanRestart:    f.phase == webLoginError,
	}
	switch f.promptKind {
	case monitor.LoginPromptCode:
		view.NeedsInput = true
		view.InputLabel = "Login code"
		view.InputType = "text"
		view.InputMode = "numeric"
		view.Autocomplete = "one-time-code"
		view.ButtonLabel = "Continue"
	case monitor.LoginPromptPassword:
		view.NeedsInput = true
		view.InputLabel = "2FA password"
		view.InputType = "password"
		view.Autocomplete = "current-password"
		view.ButtonLabel = "Authorize"
	case monitor.LoginPromptPhone:
		view.NeedsInput = true
		view.InputLabel = "Phone"
		view.InputType = "tel"
		view.Autocomplete = "tel"
		view.ButtonLabel = "Continue"
	}
	return view
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	view, err := s.loginView(r.Context(), token, false)
	if err != nil {
		view = webLoginView{Token: token, Phase: webLoginError, Error: err.Error()}
	}
	view.PageError = r.URL.Query().Get("error")
	s.renderLogin(w, view)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, webLoginView{Phase: webLoginError, Error: "invalid form"})
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	flow := s.login.get(token)
	if flow == nil {
		http.Redirect(w, r, "/login?token="+url.QueryEscape(token), http.StatusSeeOther)
		return
	}
	if err := flow.submit(r.FormValue("value")); err != nil {
		view := flow.snapshot()
		view.PageError = err.Error()
		s.renderLogin(w, view)
		return
	}
	http.Redirect(w, r, "/login?token="+url.QueryEscape(token), http.StatusSeeOther)
}

func (s *Server) loginRestart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, webLoginView{Phase: webLoginError, Error: "invalid form"})
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	view, err := s.loginView(r.Context(), token, true)
	if err != nil {
		view = webLoginView{Token: token, Phase: webLoginError, Error: err.Error()}
		s.renderLogin(w, view)
		return
	}
	view.Notice = "New code requested."
	s.renderLogin(w, view)
}

func (s *Server) loginView(ctx context.Context, token string, restart bool) (webLoginView, error) {
	if token == "" {
		return webLoginView{}, errors.New("login link is missing token")
	}
	if !restart {
		if flow := s.login.get(token); flow != nil {
			return flow.snapshot(), nil
		}
	}

	record, err := s.validLoginToken(ctx, token)
	if err != nil {
		return webLoginView{}, err
	}

	var flow *webLoginFlow
	if restart {
		flow, err = s.login.restart(record)
	} else {
		flow, err = s.login.start(record)
	}
	if err != nil {
		return webLoginView{}, err
	}
	return flow.snapshot(), nil
}

func (s *Server) validLoginToken(ctx context.Context, token string) (db.LoginToken, error) {
	record, ok, err := s.store.GetLoginToken(ctx, token)
	if err != nil {
		return db.LoginToken{}, err
	}
	if !ok {
		return db.LoginToken{}, errors.New("login link is invalid")
	}
	if !record.UsedAt.IsZero() {
		return db.LoginToken{}, errors.New("login link was already used")
	}
	if record.ExpiresAt.IsZero() || time.Now().UTC().After(record.ExpiresAt) {
		return db.LoginToken{}, errors.New("login link expired. Send /login again")
	}
	return record, nil
}

func (s *Server) renderLogin(w http.ResponseWriter, view webLoginView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.loginTemplate.Execute(w, view); err != nil {
		log.Printf("render login failed: %v", err)
	}
}

func webLoginErrorMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Timed out. Send /login again."
	}
	if errors.Is(err, context.Canceled) {
		return "Cancelled."
	}
	if errors.Is(err, monitor.ErrAccountMismatch) {
		return "This is a different Telegram account. Log in with the account you use to talk to the bot."
	}
	if errors.Is(err, monitor.ErrLoginInProgress) || errors.Is(err, monitor.ErrLoginBusy) {
		return err.Error()
	}
	detail := err.Error()
	switch {
	case strings.Contains(detail, "PHONE_CODE_EXPIRED"):
		return "Code expired. Send new code."
	case strings.Contains(detail, "PHONE_CODE_INVALID"):
		return "Bad code. Send new code."
	case strings.Contains(detail, "PHONE_NUMBER_INVALID"):
		return "Bad phone. Send /login again."
	case strings.Contains(detail, "PASSWORD_HASH_INVALID"):
		return "Bad 2FA password. Send new code."
	case strings.Contains(detail, "FLOOD_WAIT"):
		return "Telegram rate limit. Wait and send /login again."
	default:
		return fmt.Sprintf("Telegram login failed: %s", detail)
	}
}

func (m *webLoginManager) notifyLogin(chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.notifier.NotifyUser(ctx, chatID, "Telegram подключён. Теперь можно подключить MCP и настроить радар."); err != nil {
		log.Printf("send login notification failed: %v", err)
	}
}

const loginTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  {{if .AutoRefresh}}<meta http-equiv="refresh" content="2">{{end}}
  <title>telegram-bridge login</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f6f7f9;
      --panel: #ffffff;
      --text: #17202a;
      --muted: #637083;
      --line: #d9dee7;
      --accent: #0f766e;
      --danger: #b42318;
      --warn: #a15c00;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: var(--bg);
      color: var(--text);
      font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    main {
      width: min(440px, calc(100% - 32px));
      margin: 12vh auto 0;
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 18px;
    }
    h1 {
      margin: 0 0 4px;
      font-size: 22px;
      line-height: 1.15;
    }
    p { margin: 10px 0 0; }
    label {
      display: block;
      margin-top: 16px;
      color: var(--muted);
      font-size: 13px;
    }
    input {
      width: 100%;
      min-height: 44px;
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 0 12px;
      margin-top: 6px;
      font: inherit;
      background: #fff;
    }
    button, .button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-height: 42px;
      border: 1px solid var(--accent);
      border-radius: 6px;
      background: var(--accent);
      color: #fff;
      padding: 0 13px;
      margin-top: 14px;
      font: inherit;
      text-decoration: none;
      cursor: pointer;
    }
    button.secondary {
      border-color: var(--line);
      background: #fff;
      color: var(--text);
    }
    .muted { color: var(--muted); }
    .badge {
      display: inline-flex;
      align-items: center;
      min-height: 24px;
      border-radius: 999px;
      background: #e6f3f1;
      color: #0f5f59;
      padding: 0 9px;
      font-size: 12px;
      font-weight: 650;
      margin-top: 14px;
    }
    .badge.warn {
      background: #fff4df;
      color: var(--warn);
    }
    .badge.error {
      background: #fff0ee;
      color: var(--danger);
    }
    .alert {
      border-radius: 6px;
      padding: 10px 12px;
      margin-top: 14px;
    }
    .error-box { background: #fff0ee; color: var(--danger); border: 1px solid #ffc9c2; }
    .notice-box { background: #edfdf8; color: #0b5f56; border: 1px solid #bce7dd; }
    .actions {
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
      align-items: center;
    }
  </style>
</head>
<body>
  <main>
    <section class="panel">
      <h1>telegram-bridge login</h1>
      {{if .Phone}}<p class="muted">{{.Phone}}</p>{{end}}

      {{if .PageError}}<div class="alert error-box">{{.PageError}}</div>{{end}}
      {{if .Notice}}<div class="alert notice-box">{{.Notice}}</div>{{end}}

      {{if eq .Phase "authorized"}}
        <span class="badge">Authorized</span>
        {{if .UserID}}<p class="muted">user_id={{.UserID}}</p>{{end}}
        <a class="button" href="/">Open dashboard</a>
      {{else if eq .Phase "prompt"}}
        <span class="badge warn">{{.PromptMessage}}</span>
        <form method="post" action="/login" autocomplete="off">
          <input type="hidden" name="token" value="{{.Token}}">
          <label for="value">{{.InputLabel}}</label>
          <input id="value" name="value" type="{{.InputType}}" autocomplete="{{.Autocomplete}}" {{if .InputMode}}inputmode="{{.InputMode}}"{{end}} required autofocus>
          <button type="submit">{{.ButtonLabel}}</button>
        </form>
      {{else if eq .Phase "starting"}}
        <span class="badge warn">{{.PromptMessage}}</span>
        <p class="muted">Keep this page open.</p>
      {{else}}
        <span class="badge error">Login failed</span>
        {{if .Error}}<div class="alert error-box">{{.Error}}</div>{{end}}
        {{if .CanRestart}}
          <form method="post" action="/login/restart">
            <input type="hidden" name="token" value="{{.Token}}">
            <button type="submit">Send new code</button>
          </form>
        {{else}}
          <p class="muted">Send /login in the bot.</p>
        {{end}}
      {{end}}
    </section>
  </main>
</body>
</html>`
