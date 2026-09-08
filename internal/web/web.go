package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nextster/telegram-bridge/internal/bot"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/mcpserver"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/notify"
)

type Server struct {
	cfg           config.Config
	store         *db.Store
	monitor       *monitor.Service
	notifier      notify.SystemNotifier
	template      *template.Template
	loginTemplate *template.Template
	login         *webLoginManager
	codexNotifier CodexNotifier
}

type CodexNotifier interface {
	SendCodexJobResult(context.Context, db.CodexJob) error
	CreateCodexTask(context.Context, string, string) (bot.CodexTask, error)
	DeleteArchivedCodexTopics(context.Context, []string) (int, error)
	SyncCodexThreads(context.Context, []db.CodexThreadSnapshot) (bot.CodexSyncResult, error)
}

type dashboardData struct {
	Stats          db.Stats
	Keywords       []db.Keyword
	Peers          []db.MonitorPeer
	Events         []db.Event
	TelegramStatus monitor.Status
	Error          string
	Notice         string
	Now            time.Time
}

func New(cfg config.Config, store *db.Store, monitorService *monitor.Service, notifier notify.SystemNotifier, codexNotifier CodexNotifier) (*Server, error) {
	if notifier == nil {
		notifier = notify.Nop{}
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
		monitor:       monitorService,
		notifier:      notifier,
		template:      tpl,
		loginTemplate: loginTpl,
		login:         newWebLoginManager(store, monitorService, notifier),
		codexNotifier: codexNotifier,
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

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboardEntry)
	mux.HandleFunc("POST /webapp/auth", s.authenticateWebApp)
	mux.HandleFunc("POST /keywords/add", s.requireWebAdmin(s.addKeyword))
	mux.HandleFunc("POST /rules/add", s.requireWebAdmin(s.addWatchRule))
	mux.HandleFunc("POST /keywords/delete", s.requireWebAdmin(s.deleteKeyword))
	mux.HandleFunc("POST /sources/sync", s.requireWebAdmin(s.syncSources))
	mux.HandleFunc("POST /sources/toggle", s.requireWebAdmin(s.toggleSource))
	mux.HandleFunc("POST /history/backfill", s.requireWebAdmin(s.backfillHistory))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /login/restart", s.loginRestart)
	mux.HandleFunc("GET /healthz", s.healthz)
	if s.cfg.HasMCP() && s.monitor != nil {
		var notifications *notify.Notifications
		if sender, ok := s.notifier.(notify.NotificationSender); ok && s.store != nil && len(s.cfg.NotificationChatIDs) > 0 {
			notifications = notify.NewNotifications(s.store, sender, s.cfg.NotificationChatIDs)
		}
		mux.Handle("/mcp", mcpserver.New(s.monitor, s.cfg.MCPToken, notifications))
		log.Print("MCP endpoint enabled at /mcp")
	}
	if s.cfg.HasWorkerAPI() {
		mux.HandleFunc("POST /worker/v1/jobs/claim", s.requireWorker(s.claimCodexJob))
		mux.HandleFunc("POST /worker/v1/jobs/{id}/start", s.requireWorker(s.startCodexJob))
		mux.HandleFunc("POST /worker/v1/jobs/{id}/finish", s.requireWorker(s.finishCodexJob))
		mux.HandleFunc("POST /worker/v1/jobs/{id}/retry", s.requireWorker(s.retryCodexJob))
		mux.HandleFunc("POST /worker/v1/tasks", s.requireWorker(s.createCodexTask))
		mux.HandleFunc("POST /worker/v1/archive-sync", s.requireWorker(s.syncCodexArchives))
		mux.HandleFunc("POST /worker/v1/thread-sync", s.requireWorker(s.syncCodexThreads))
		mux.HandleFunc("POST /worker/v1/read-receipts/claim", s.requireWorker(s.claimCodexReadReceipts))
		mux.HandleFunc("POST /worker/v1/read-receipts/ack", s.requireWorker(s.ackCodexReadReceipts))
		log.Print("Codex worker API enabled at /worker/v1")
	}
	return mux
}

func (s *Server) claimCodexReadReceipts(w http.ResponseWriter, r *http.Request) {
	ids, err := s.store.PendingCodexReadReceipts(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeWorkerJSON(w, http.StatusOK, map[string]any{"thread_ids": ids})
}

func (s *Server) ackCodexReadReceipts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ThreadIDs []string `json:"thread_ids"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.AckCodexReadReceipts(r.Context(), request.ThreadIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncCodexThreads(w http.ResponseWriter, r *http.Request) {
	if s.codexNotifier == nil {
		http.Error(w, "Telegram bot is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Threads []db.CodexThreadSnapshot `json:"threads"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.codexNotifier.SyncCodexThreads(r.Context(), request.Threads)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeWorkerJSON(w, http.StatusOK, result)
}

func (s *Server) syncCodexArchives(w http.ResponseWriter, r *http.Request) {
	if s.codexNotifier == nil {
		http.Error(w, "Telegram bot is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		ThreadIDs []string `json:"thread_ids"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deleted, err := s.codexNotifier.DeleteArchivedCodexTopics(r.Context(), request.ThreadIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeWorkerJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}

func (s *Server) retryCodexJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RetryCodexJob(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createCodexTask(w http.ResponseWriter, r *http.Request) {
	if s.codexNotifier == nil {
		http.Error(w, "Telegram bot is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Project string `json:"project"`
		Prompt  string `json:"prompt"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	task, err := s.codexNotifier.CreateCodexTask(r.Context(), request.Project, request.Prompt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeWorkerJSON(w, http.StatusCreated, task)
}

func (s *Server) requireWorker(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		expected := strings.TrimSpace(s.cfg.WorkerToken)
		if auth == "" || len(auth) != len(expected) || subtle.ConstantTimeCompare([]byte(auth), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func (s *Server) claimCodexJob(w http.ResponseWriter, r *http.Request) {
	var request struct {
		WorkerID string `json:"worker_id"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	job, ok, err := s.store.ClaimCodexJob(r.Context(), request.WorkerID, 30*time.Minute)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeWorkerJSON(w, http.StatusOK, job)
}

func (s *Server) startCodexJob(w http.ResponseWriter, r *http.Request) {
	var request struct {
		LeaseToken    string `json:"lease_token"`
		CodexThreadID string `json:"codex_thread_id"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.StartCodexJob(r.Context(), r.PathValue("id"), request.LeaseToken, request.CodexThreadID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finishCodexJob(w http.ResponseWriter, r *http.Request) {
	var request struct {
		LeaseToken string `json:"lease_token"`
		Result     string `json:"result"`
		Error      string `json:"error"`
	}
	if err := decodeWorkerJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	job, err := s.store.FinishCodexJob(r.Context(), r.PathValue("id"), request.LeaseToken, request.Result, request.Error)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if s.codexNotifier != nil {
		if err := s.codexNotifier.SendCodexJobResult(r.Context(), job); err != nil {
			log.Printf("send Codex result to Telegram failed: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeWorkerJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeWorkerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode worker response failed: %v", err)
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors https://web.telegram.org https://*.telegram.org")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	stats, err := s.store.Stats(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	keywords, err := s.store.ListKeywords(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	peers, err := s.store.ListMonitorPeers(ctx, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	events, err := s.store.ListEvents(ctx, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := dashboardData{
		Stats:    stats,
		Keywords: keywords,
		Peers:    peers,
		Events:   events,
		Error:    r.URL.Query().Get("error"),
		Notice:   r.URL.Query().Get("notice"),
		Now:      time.Now(),
	}
	if s.monitor != nil {
		data.TelegramStatus = s.monitor.Status()
	} else {
		data.TelegramStatus = monitor.Status{Configured: s.cfg.HasTelegramUserAPI()}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.template.Execute(w, data); err != nil {
		log.Printf("render dashboard failed: %v", err)
	}
}

func (s *Server) addKeyword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	phrase := r.FormValue("phrase")
	keyword, err := s.store.AddKeyword(r.Context(), phrase)
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	s.notifySystem(fmt.Sprintf("Keyword added: #%d %s", keyword.ID, keyword.Phrase))
	redirectNotice(w, r, fmt.Sprintf("added keyword #%d", keyword.ID))
}

func (s *Server) addWatchRule(w http.ResponseWriter, r *http.Request) {
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
		if err == nil && peerID > 0 {
			sources = append(sources, db.RuleSource{PeerType: peerType, PeerID: peerID})
		}
	}
	rule, err := s.store.UpsertWatchRule(r.Context(), db.Keyword{
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
	s.notifySystem(fmt.Sprintf("Watch rule added: #%d %s", rule.ID, rule.Phrase))
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

func (s *Server) deleteKeyword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectError(w, r, "invalid form")
		return
	}
	id := r.FormValue("id")
	if id == "" {
		id = r.FormValue("phrase")
	}
	rows, err := s.store.DeleteKeyword(r.Context(), id)
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

func (s *Server) syncSources(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		redirectError(w, r, "telegram monitor is not configured")
		return
	}
	count, err := s.monitor.SyncDialogs(r.Context())
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	s.notifySystem(fmt.Sprintf("Sources synced: %d chats found.", count))
	redirectNotice(w, r, fmt.Sprintf("synced %d sources from Telegram", count))
}

func (s *Server) toggleSource(w http.ResponseWriter, r *http.Request) {
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
	if err := s.store.SetMonitorPeerEnabled(r.Context(), peerType, peerID, enabled); err != nil {
		redirectError(w, r, err.Error())
		return
	}
	peer, _, _ := s.store.GetMonitorPeer(r.Context(), peerType, peerID)
	sourceName := formatSourceName(peerType, peerID, peer)
	if enabled {
		s.notifySystem("Monitoring enabled: " + sourceName)
		redirectNotice(w, r, "source monitoring enabled")
		return
	}
	s.notifySystem("Monitoring paused: " + sourceName)
	redirectNotice(w, r, "source monitoring paused")
}

func (s *Server) backfillHistory(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		redirectError(w, r, "telegram monitor is not configured")
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
	result, err := s.monitor.Backfill(r.Context(), days)
	if err != nil {
		redirectError(w, r, err.Error())
		return
	}
	s.notifySystem(fmt.Sprintf("History scan finished: %d messages, %d matched, %d new events", result.Scanned, result.Matched, result.Inserted))
	redirectNotice(w, r, fmt.Sprintf("history scanned: %d messages, %d matched, %d new events", result.Scanned, result.Matched, result.Inserted))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	stats, err := s.store.Stats(ctx)
	if err != nil {
		log.Printf("health stats unavailable: %v", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok stats=unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok subscribers=%d keywords=%d sources=%d enabled_sources=%d events=%d\n", stats.Subscribers, stats.Keywords, stats.Peers, stats.EnabledPeers, stats.Events)
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

func (s *Server) notifySystem(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.notifier.NotifySystem(ctx, text); err != nil {
		log.Printf("send system notification failed: %v", err)
	}
}

func formatSourceName(peerType string, peerID int64, peer db.MonitorPeer) string {
	if title := strings.TrimSpace(peer.Title); title != "" {
		if username := strings.TrimSpace(peer.Username); username != "" {
			return fmt.Sprintf("%s (@%s)", title, username)
		}
		return title
	}
	if username := strings.TrimSpace(peer.Username); username != "" {
		return "@" + username
	}
	return fmt.Sprintf("%s:%d", peerType, peerID)
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
    input, textarea {
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
      <div class="stat"><strong>{{.Stats.Subscribers}}</strong><span>Subscribers</span></div>
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
            <span class="badge warn">Telegram user not logged in</span>
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
