package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/nextster/telegram-bridge/internal/apitoken"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/mcpserver"
	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/notify"
	"github.com/nextster/telegram-bridge/internal/oauth"
)

// Accounts is the account manager as seen by the web server. Every method is
// scoped to one Telegram user.
type Accounts interface {
	Account(owner int64) (*monitor.Service, error)
	Status(owner int64) monitor.Status
	Login(ctx context.Context, owner int64, opts monitor.LoginOptions) (monitor.LoginResult, error)
	NotificationSender(ctx context.Context, accountID int64) (notify.NotificationSender, error)
}

// UserNotifier delivers a private message to one user.
type UserNotifier interface {
	NotifyUser(ctx context.Context, userID int64, text string) error
}

type nopUserNotifier struct{}

func (nopUserNotifier) NotifyUser(context.Context, int64, string) error { return nil }

// Server serves the per-user dashboard, site login, MCP, OAuth, and the
// notification API. Every dashboard request acts for the Telegram user in the
// signed Mini App session only.
type Server struct {
	cfg           config.Config
	store         *db.Store
	accounts      Accounts
	media         *media.Service
	notifier      UserNotifier
	template      *template.Template
	loginTemplate *template.Template
	login         *webLoginManager
	oauth         *oauth.Server
}

type dashboardData struct {
	Stats             db.Stats
	Keywords          []db.Keyword
	Peers             []db.MonitorPeer
	Events            []db.Event
	Tokens            []db.APIToken
	NotificationChats []db.NotificationChat
	NewToken          string
	NewTokenScope     string
	PublicURL         string
	TelegramStatus    monitor.Status
	Error             string
	Notice            string
	Now               time.Time
}

func New(cfg config.Config, store *db.Store, accounts Accounts, notifier UserNotifier) (*Server, error) {
	if notifier == nil {
		notifier = nopUserNotifier{}
	}
	tpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"time": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02")
		},
		"short": func(value string, limit int) string {
			value = strings.TrimSpace(value)
			runes := []rune(value)
			if len(runes) <= limit {
				return value
			}
			return string(runes[:limit]) + "..."
		},
		"join": strings.Join,
	}).Parse(dashboardTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}
	loginTpl, err := template.New("login").Parse(loginTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse login template: %w", err)
	}
	return &Server{
		cfg:           cfg,
		store:         store,
		accounts:      accounts,
		notifier:      notifier,
		template:      tpl,
		loginTemplate: loginTpl,
		login:         newWebLoginManager(store, accounts, notifier),
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:         s.cfg.Addr,
		Handler:      s.routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown failed: %v", err)
		}
	}()

	log.Printf("web server listening on %s", s.cfg.Addr)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) SetMediaService(service *media.Service) { s.media = service }

// SetOAuth enables OAuth authorization for MCP clients.
func (s *Server) SetOAuth(server *oauth.Server) { s.oauth = server }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboardEntry)
	mux.HandleFunc("POST /webapp/auth", s.authenticateWebApp)
	mux.HandleFunc("POST /keywords/add", s.requireWebUser(s.addKeyword))
	mux.HandleFunc("POST /rules/add", s.requireWebUser(s.addWatchRule))
	mux.HandleFunc("POST /keywords/delete", s.requireWebUser(s.deleteKeyword))
	mux.HandleFunc("POST /sources/sync", s.requireWebUser(s.syncSources))
	mux.HandleFunc("POST /sources/toggle", s.requireWebUser(s.toggleSource))
	mux.HandleFunc("POST /history/backfill", s.requireWebUser(s.backfillHistory))
	mux.HandleFunc("POST /tokens/create", s.requireWebUser(s.createToken))
	mux.HandleFunc("POST /tokens/delete", s.requireWebUser(s.deleteToken))
	mux.HandleFunc("POST /notification-chats/add", s.requireWebUser(s.addNotificationChat))
	mux.HandleFunc("POST /notification-chats/delete", s.requireWebUser(s.deleteNotificationChat))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /login/restart", s.loginRestart)
	mux.HandleFunc("GET /healthz", s.healthz)
	if s.accounts != nil && s.store != nil {
		options := mcpserver.Options{
			Media:       s.media,
			PublicURL:   s.cfg.PublicBaseURL,
			VerifyToken: s.verifyMCPToken,
			Accounts: func(userID int64) (mcpserver.Monitor, error) {
				account, err := s.accounts.Account(userID)
				if err != nil {
					return nil, err
				}
				return account, nil
			},
		}
		if s.oauth != nil {
			s.oauth.Register(mux)
			options.ResourceMetadataURL = s.oauth.ResourceMetadataURL()
			log.Print("MCP OAuth enabled with Telegram bot approval")
		}
		mcpHandler := mcpserver.New(options)
		mux.Handle("/mcp", mcpHandler)
		mux.Handle("/mcp/media/", mcpHandler)
		log.Print("MCP endpoint enabled at /mcp")

		notifications := notify.NewNotifications(s.store, s.accounts)
		mux.Handle("POST /notifications/v1/messages", notify.NewHTTPHandler(notifications, s.store))
		log.Print("Notification API enabled at /notifications/v1/messages")
	}
	return mux
}

// verifyMCPToken accepts OAuth access tokens and personal MCP tokens and
// returns the Telegram user they belong to.
func (s *Server) verifyMCPToken(ctx context.Context, token string) (int64, time.Time, error) {
	now := time.Now()
	if s.oauth != nil {
		userID, expires, ok, err := s.oauth.VerifyAccessToken(ctx, token)
		if err != nil {
			return 0, time.Time{}, err
		}
		if ok {
			return userID, expires, nil
		}
	}
	owner, ok, err := s.store.ResolveAPIToken(ctx, apitoken.Hash(token), db.APITokenScopeMCP, now)
	if err != nil {
		return 0, time.Time{}, err
	}
	if !ok {
		return 0, time.Time{}, auth.ErrInvalidToken
	}
	return owner, now.Add(time.Hour), nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, userID int64) {
	s.renderDashboard(w, r, userID, dashboardData{
		Error:  r.URL.Query().Get("error"),
		Notice: r.URL.Query().Get("notice"),
	})
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, userID int64, data dashboardData) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors https://web.telegram.org https://*.telegram.org")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var err error
	if data.Stats, err = s.store.Stats(ctx, userID); err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	if data.Keywords, err = s.store.ListKeywords(ctx, userID); err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	if data.Peers, err = s.store.ListMonitorPeers(ctx, userID, false); err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	if data.Events, err = s.store.ListEvents(ctx, userID, 50); err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	if data.Tokens, err = s.store.ListAPITokens(ctx, userID); err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	if data.NotificationChats, err = s.store.ListNotificationChats(ctx, userID); err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	data.Now = time.Now()
	data.PublicURL = strings.TrimRight(s.cfg.PublicBaseURL, "/")
	if s.accounts != nil {
		data.TelegramStatus = s.accounts.Status(userID)
	} else {
		data.TelegramStatus = monitor.Status{Configured: s.cfg.HasTelegramUserAPI()}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.template.Execute(w, data); err != nil {
		log.Printf("render dashboard failed: %v", err)
	}
}

func (s *Server) addKeyword(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	keyword, err := s.store.AddKeyword(r.Context(), userID, r.FormValue("phrase"))
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("added keyword #%d", keyword.ID))
}

func (s *Server) addWatchRule(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	sources := make([]db.RuleSource, 0, len(r.Form["sources"]))
	for _, raw := range r.Form["sources"] {
		peerType, rawID, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok {
			continue
		}
		peerID, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || peerID <= 0 {
			continue
		}
		// Rules may only be scoped to the user's own sources.
		if _, owned, err := s.store.GetMonitorPeer(r.Context(), userID, peerType, peerID); err == nil && owned {
			sources = append(sources, db.RuleSource{PeerType: peerType, PeerID: peerID})
		}
	}
	rule, err := s.store.UpsertWatchRule(r.Context(), userID, db.Keyword{
		Phrase:              r.FormValue("name"),
		AnyTerms:            splitTerms(r.FormValue("any")),
		AllTerms:            splitTerms(r.FormValue("all")),
		RequiredAnyGroups:   splitTermGroups(r.FormValue("required_any")),
		PreferredTerms:      splitTerms(r.FormValue("prefer")),
		ExcludeTerms:        splitTerms(r.FormValue("exclude")),
		Note:                r.FormValue("note"),
		ExcludeCompleteBike: r.FormValue("exclude_complete_bike") == "1",
		Sources:             sources,
		Enabled:             true,
	})
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("watching %s", rule.Phrase))
}

func splitTerms(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitTermGroups(value string) [][]string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	out := make([][]string, 0, len(lines))
	for _, line := range lines {
		if terms := splitTerms(line); len(terms) > 0 {
			out = append(out, terms)
		}
	}
	return out
}

func (s *Server) deleteKeyword(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	id := r.FormValue("id")
	if id == "" {
		id = r.FormValue("phrase")
	}
	rows, err := s.store.DeleteKeyword(r.Context(), userID, id)
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	if rows == 0 {
		redirectError(w, r, "keyword not found")
		return
	}
	redirectNotice(w, r, "deleted keyword")
}

func (s *Server) account(userID int64) (*monitor.Service, error) {
	if s.accounts == nil {
		return nil, errors.New("telegram monitor is not configured")
	}
	account, err := s.accounts.Account(userID)
	if err != nil {
		return nil, errors.New("your Telegram account is not connected; send /login to the bot")
	}
	return account, nil
}

func (s *Server) syncSources(w http.ResponseWriter, r *http.Request, userID int64) {
	account, err := s.account(userID)
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	count, err := account.SyncDialogs(r.Context())
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("synced %d sources from Telegram", count))
}

func (s *Server) toggleSource(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	peerType := r.FormValue("peer_type")
	peerID, err := strconv.ParseInt(r.FormValue("peer_id"), 10, 64)
	if err != nil {
		redirectError(w, r, "invalid peer id")
		return
	}
	enabled := r.FormValue("enabled") == "1"
	if err := s.store.SetMonitorPeerEnabled(r.Context(), userID, peerType, peerID, enabled); err != nil {
		redirectError(w, r, "source not found")
		return
	}
	if enabled {
		redirectNotice(w, r, "source monitoring enabled")
		return
	}
	redirectNotice(w, r, "source monitoring paused")
}

func (s *Server) backfillHistory(w http.ResponseWriter, r *http.Request, userID int64) {
	account, err := s.account(userID)
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	days, err := strconv.Atoi(r.FormValue("days"))
	if err != nil {
		redirectError(w, r, "invalid days value")
		return
	}
	result, err := account.Backfill(r.Context(), days)
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("history scanned: %d messages, %d matched, %d new events", result.Scanned, result.Matched, result.Inserted))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	scope := r.FormValue("scope")
	token, hash, err := apitoken.New(scope)
	if err != nil {
		redirectError(w, r, "unknown token type")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	if _, err := s.store.CreateAPIToken(r.Context(), userID, scope, name, hash, time.Now()); err != nil {
		redirectError(w, r, "could not create token")
		return
	}
	// The token is shown once, in this response only.
	s.renderDashboard(w, r, userID, dashboardData{NewToken: token, NewTokenScope: scope, Notice: "token created; copy it now"})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || s.store.DeleteAPIToken(r.Context(), userID, id) != nil {
		redirectError(w, r, "token not found")
		return
	}
	redirectNotice(w, r, "token deleted")
}

func (s *Server) addNotificationChat(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	chatID, err := parseNotificationChat(r.FormValue("chat"))
	if err != nil {
		redirectError(w, r, "use a group key such as channel:123 or a Bot API group ID such as -100123")
		return
	}
	if s.accounts == nil {
		redirectError(w, r, "telegram monitor is not configured")
		return
	}
	sender, err := s.accounts.NotificationSender(r.Context(), userID)
	if err != nil {
		redirectError(w, r, "your Telegram account is not connected; send /login to the bot")
		return
	}
	// The group must be reachable by the user's own account.
	if err := sender.CheckNotificationChat(r.Context(), chatID); err != nil {
		redirectError(w, r, "your Telegram account cannot post in that group")
		return
	}
	if err := s.store.AddNotificationChat(r.Context(), userID, chatID, r.FormValue("title"), time.Now()); err != nil {
		redirectError(w, r, "could not save group")
		return
	}
	redirectNotice(w, r, "notification group allowed")
}

func (s *Server) deleteNotificationChat(w http.ResponseWriter, r *http.Request, userID int64) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	chatID, err := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	if err != nil || s.store.RemoveNotificationChat(r.Context(), userID, chatID) != nil {
		redirectError(w, r, "group not found")
		return
	}
	redirectNotice(w, r, "notification group removed")
}

// parseNotificationChat accepts a group key (channel:123, chat:123) or a Bot
// API group ID (-100123, -123).
func parseNotificationChat(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, ":") {
		return notify.ChatID(value)
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id >= 0 {
		return 0, errors.New("invalid group id")
	}
	if id <= -1000000000000 {
		return notify.ChatID("channel:" + strconv.FormatInt(-1000000000000-id, 10))
	}
	return notify.ChatID("chat:" + strconv.FormatInt(-id, 10))
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func redirectError(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?error="+urlQuery(message), http.StatusSeeOther)
}

func redirectNotice(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?notice="+urlQuery(message), http.StatusSeeOther)
}

func urlQuery(value string) string {
	return url.QueryEscape(value)
}

const dashboardTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>telegram-bridge</title>
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
    header {
      border-bottom: 1px solid var(--line);
      background: var(--panel);
    }
    .wrap {
      width: min(1120px, calc(100% - 32px));
      margin: 0 auto;
    }
    .top {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      min-height: 64px;
    }
    h1 {
      margin: 0;
      font-size: 22px;
      line-height: 1;
    }
    h2 {
      margin: 0 0 14px;
      font-size: 16px;
    }
    main {
      padding: 24px 0 40px;
    }
    .stats {
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 12px;
      margin-bottom: 18px;
    }
    .stat, .panel, .event, .keyword, .source {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
    }
    .stat {
      padding: 14px;
    }
    .stat strong {
      display: block;
      font-size: 26px;
      line-height: 1.1;
    }
    .stat span, .muted {
      color: var(--muted);
    }
    .grid {
      display: grid;
      grid-template-columns: minmax(0, 1fr) 340px;
      gap: 18px;
      align-items: start;
      margin-bottom: 18px;
    }
    .history-grid {
      grid-template-columns: 340px minmax(0, 1fr);
    }
    .panel {
      padding: 16px;
    }
    .form-row {
      display: flex;
      gap: 8px;
    }
    input, textarea, select {
      width: 100%;
      min-height: 40px;
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 0 10px;
      font: inherit;
      background: #fff;
    }
    textarea {
      min-height: 72px;
      padding: 9px 10px;
      resize: vertical;
    }
    button {
      min-height: 40px;
      border: 1px solid var(--accent);
      border-radius: 6px;
      background: var(--accent);
      color: #fff;
      padding: 0 12px;
      font: inherit;
      cursor: pointer;
      white-space: nowrap;
    }
    button.delete {
      border-color: var(--line);
      background: #fff;
      color: var(--danger);
    }
    input[type="number"] {
      max-width: 110px;
    }
    .keyword, .source {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 10px 12px;
      margin-top: 10px;
    }
    .keyword code, .source-main {
      overflow-wrap: anywhere;
    }
    .keyword:target {
      scroll-margin-top: 16px;
      border-color: var(--accent);
      box-shadow: 0 0 0 2px rgba(15, 118, 110, .18);
    }
    .rule-form { display: grid; gap: 10px; margin-bottom: 16px; }
    .rule-form label { display: grid; gap: 5px; color: var(--muted); font-size: 12px; }
    .source-picker { display: flex; flex-wrap: wrap; gap: 7px 12px; }
    .source-picker label { display: flex; align-items: center; gap: 5px; color: var(--text); }
    .source-picker input { width: auto; min-height: 0; }
    .rule-copy { display: grid; gap: 3px; min-width: 0; }
    .rule-copy small { color: var(--muted); overflow-wrap: anywhere; }
    .source-actions {
      display: flex;
      gap: 8px;
      align-items: center;
      flex-shrink: 0;
    }
    .status-line {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 8px;
      margin: 0 0 12px;
    }
    .events {
      display: grid;
      gap: 10px;
    }
    .event {
      padding: 12px;
    }
    .event-head {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 8px;
    }
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
    }
    .badge.off {
      background: #f2f4f7;
      color: var(--muted);
    }
    .badge.warn {
      background: #fff4df;
      color: var(--warn);
    }
    pre {
      margin: 0;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
      font: inherit;
    }
    .alert {
      border-radius: 6px;
      padding: 10px 12px;
      margin-bottom: 14px;
    }
    .error { background: #fff0ee; color: var(--danger); border: 1px solid #ffc9c2; }
    .notice { background: #edfdf8; color: #0b5f56; border: 1px solid #bce7dd; }
    @media (max-width: 820px) {
      .stats, .grid { grid-template-columns: 1fr; }
      .top { align-items: flex-start; flex-direction: column; padding: 16px 0; }
    }
  </style>
</head>
<body>
  <header>
    <div class="wrap top">
      <h1>telegram-bridge</h1>
      <div class="muted">Updated {{time .Now}}</div>
    </div>
  </header>
  <main class="wrap">
    {{if .Error}}<div class="alert error">{{.Error}}</div>{{end}}
    {{if .Notice}}<div class="alert notice">{{.Notice}}</div>{{end}}

    <section class="stats">
      <div class="stat"><strong>{{.Stats.Peers}}</strong><span>Known sources</span></div>
      <div class="stat"><strong>{{.Stats.Keywords}}</strong><span>Watch rules</span></div>
      <div class="stat"><strong>{{.Stats.EnabledPeers}}</strong><span>Monitored Sources</span></div>
      <div class="stat"><strong>{{.Stats.Events}}</strong><span>Matches</span></div>
    </section>

    <section class="grid">
      <div class="panel">
        <h2>Sources</h2>
        <div class="status-line">
          {{if .TelegramStatus.Authorized}}
            <span class="badge">Telegram user connected</span>
            <span class="muted">user {{.TelegramStatus.UserID}}</span>
          {{else if .TelegramStatus.Configured}}
            <span class="badge warn">Telegram not connected · send /login to the bot</span>
          {{else}}
            <span class="badge off">Telegram user API not configured</span>
          {{end}}
        </div>
        <form method="post" action="/sources/sync" class="form-row">
          <button type="submit">Refresh from Telegram</button>
        </form>
        {{range .Peers}}
          <div class="source">
            <div class="source-main">
              <strong>{{.Title}}</strong>
              <div class="muted">{{.Kind}} {{if .Username}}@{{.Username}}{{else}}{{.PeerType}}:{{.PeerID}}{{end}}{{if .LastBackfillAt}} · last scan {{date .LastBackfillAt}}{{end}}</div>
            </div>
            <div class="source-actions">
              {{if .Enabled}}<span class="badge">On</span>{{else}}<span class="badge off">Off</span>{{end}}
              <form method="post" action="/sources/toggle">
                <input type="hidden" name="peer_type" value="{{.PeerType}}">
                <input type="hidden" name="peer_id" value="{{.PeerID}}">
                {{if .Enabled}}
                  <input type="hidden" name="enabled" value="0">
                  <button class="delete" type="submit">Pause</button>
                {{else}}
                  <input type="hidden" name="enabled" value="1">
                  <button type="submit">Monitor</button>
                {{end}}
              </form>
            </div>
          </div>
        {{else}}
          <p class="muted">No sources synced yet. Connect Telegram user session, then refresh.</p>
        {{end}}
      </div>

      <div class="panel">
        <h2>Watch rules</h2>
        <form method="post" action="/rules/add" class="rule-form">
          <label>Name<input name="name" placeholder="repair stand" required></label>
          <label>Match any (comma or newline separated)<textarea name="any" placeholder="ремонтная стойка, workstand, prepstand"></textarea></label>
          <label>Require all<input name="all" placeholder="optional required terms"></label>
          <label>Require one from each line<textarea name="required_any" placeholder="USB-C, Type-C&#10;num&gt;=1000:lm|lumen|люмен|лм"></textarea></label>
          <label>Prefer (adds evidence, does not block)<textarea name="prefer" placeholder="daytime flash, Garmin mount"></textarea></label>
          <label>Exclude<input name="exclude" placeholder="продано, sold, куплю"></label>
          <label>Rule note<textarea name="note" placeholder="Useful specs or checks"></textarea></label>
          <label><input type="checkbox" name="exclude_complete_bike" value="1"> Ignore complete-bike listings</label>
          <div class="source-picker">
            {{range .Peers}}{{if .Enabled}}
              <label><input type="checkbox" name="sources" value="{{.PeerType}}:{{.PeerID}}">{{if .Title}}{{.Title}}{{else}}{{.PeerType}}:{{.PeerID}}{{end}}</label>
            {{end}}{{end}}
          </div>
          <small class="muted">No source selected means every enabled source.</small>
          <button type="submit">Start watching</button>
        </form>
        {{range .Keywords}}
          <div class="keyword" id="rule-{{.ID}}">
            <div class="rule-copy">
              <code>#{{.ID}} {{.Phrase}}</code>
              {{if .AnyTerms}}<small>any: {{join .AnyTerms ", "}}</small>{{end}}
              {{if .AllTerms}}<small>all: {{join .AllTerms ", "}}</small>{{end}}
              {{range .RequiredAnyGroups}}<small>one of: {{join . ", "}}</small>{{end}}
              {{if .PreferredTerms}}<small>prefer: {{join .PreferredTerms ", "}}</small>{{end}}
              {{if .ExcludeTerms}}<small>exclude: {{join .ExcludeTerms ", "}}</small>{{end}}
              {{if .Note}}<small>note: {{.Note}}</small>{{end}}
              {{if .ExcludeCompleteBike}}<small>ignores complete-bike listings</small>{{end}}
              {{if .Sources}}<small>scoped to {{len .Sources}} source(s)</small>{{end}}
            </div>
            <form method="post" action="/keywords/delete">
              <input type="hidden" name="id" value="{{.ID}}">
              <button class="delete" type="submit">Delete</button>
            </form>
          </div>
        {{else}}
          <p class="muted">No watch rules yet.</p>
        {{end}}
      </div>
    </section>

    <section class="grid">
      <div class="panel">
        <h2>Personal tokens</h2>
        {{if .NewToken}}
          <div class="alert notice">
            <strong>Copy this {{.NewTokenScope}} token now. It is not shown again.</strong>
            <pre><code>{{.NewToken}}</code></pre>
          </div>
        {{end}}
        <p class="muted">Tokens act only for your Telegram account. MCP clients that support OAuth do not need a token: add <code>{{.PublicURL}}/mcp</code> and approve the request in the bot.</p>
        <form method="post" action="/tokens/create" class="form-row">
          <input name="name" maxlength="60" placeholder="token name">
          <select name="scope">
            <option value="mcp">MCP</option>
            <option value="notify">Notifications</option>
          </select>
          <button type="submit">Create</button>
        </form>
        {{range .Tokens}}
          <div class="keyword">
            <div class="rule-copy">
              <code>#{{.ID}} {{.Scope}}{{if .Name}} · {{.Name}}{{end}}</code>
              <small>created {{date .CreatedAt}}{{if not .LastUsedAt.IsZero}} · last used {{time .LastUsedAt}}{{end}}</small>
            </div>
            <form method="post" action="/tokens/delete">
              <input type="hidden" name="id" value="{{.ID}}">
              <button class="delete" type="submit">Delete</button>
            </form>
          </div>
        {{else}}
          <p class="muted">No tokens.</p>
        {{end}}
      </div>

      <div class="panel">
        <h2>Notification groups</h2>
        <p class="muted">Notification tokens post only to groups listed here, as your Telegram account. Endpoint: <code>POST {{.PublicURL}}/notifications/v1/messages</code></p>
        <form method="post" action="/notification-chats/add" class="rule-form">
          <label>Group<input name="chat" placeholder="channel:123 or -100123" required></label>
          <label>Label<input name="title" maxlength="80" placeholder="optional"></label>
          <button type="submit">Allow group</button>
        </form>
        {{range .NotificationChats}}
          <div class="keyword">
            <div class="rule-copy">
              <code>{{.ChatID}}</code>
              {{if .Title}}<small>{{.Title}}</small>{{end}}
            </div>
            <form method="post" action="/notification-chats/delete">
              <input type="hidden" name="chat_id" value="{{.ChatID}}">
              <button class="delete" type="submit">Remove</button>
            </form>
          </div>
        {{else}}
          <p class="muted">No groups allowed.</p>
        {{end}}
      </div>
    </section>

    <section class="grid history-grid">
      <div class="panel">
        <h2>History Scan</h2>
        <form method="post" action="/history/backfill" class="form-row">
          <input type="number" name="days" min="1" max="365" value="7" required>
          <button type="submit">Scan enabled sources</button>
        </form>
        <p class="muted">Scans previous messages with current watch rules. Old matches are stored, not sent as Telegram alerts.</p>
      </div>

      <div class="panel">
        <h2>Recent Matches</h2>
        <div class="events">
        {{range .Events}}
          <article class="event">
            <div class="event-head">
              <span class="badge">{{.Keyword}}</span>
              <span class="muted">{{time .CreatedAt}}</span>
            </div>
            <div class="muted">{{.SourcePeerType}}:{{.SourcePeerID}} message {{.MessageID}}</div>
            {{if .MatchReason}}<div class="muted">{{.MatchReason}} · score {{.MatchScore}}</div>{{end}}
            <pre>{{short .Text 700}}</pre>
          </article>
        {{else}}
          <p class="muted">No matches yet.</p>
        {{end}}
        </div>
      </div>
    </section>
  </main>
</body>
</html>`
