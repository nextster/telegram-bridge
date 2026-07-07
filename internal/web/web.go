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

	"github.com/nextster/tg-radar/internal/config"
	"github.com/nextster/tg-radar/internal/db"
	"github.com/nextster/tg-radar/internal/monitor"
	"github.com/nextster/tg-radar/internal/notify"
)

type Server struct {
	cfg           config.Config
	store         *db.Store
	monitor       *monitor.Service
	notifier      notify.SystemNotifier
	template      *template.Template
	loginTemplate *template.Template
	login         *webLoginManager
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

func New(cfg config.Config, store *db.Store, monitorService *monitor.Service, notifier notify.SystemNotifier) (*Server, error) {
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
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.dashboard)
	mux.HandleFunc("POST /keywords/add", s.addKeyword)
	mux.HandleFunc("POST /keywords/delete", s.deleteKeyword)
	mux.HandleFunc("POST /sources/sync", s.syncSources)
	mux.HandleFunc("POST /sources/toggle", s.toggleSource)
	mux.HandleFunc("POST /history/backfill", s.backfillHistory)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /login/restart", s.loginRestart)
	mux.HandleFunc("GET /healthz", s.healthz)

	server := &http.Server{
		Addr:         s.cfg.Addr,
		Handler:      mux,
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

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
  <title>tg-radar</title>
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
    input {
      width: 100%;
      min-height: 40px;
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 0 10px;
      font: inherit;
      background: #fff;
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
      <h1>tg-radar</h1>
      <div class="muted">Updated {{time .Now}}</div>
    </div>
  </header>
  <main class="wrap">
    {{if .Error}}<div class="alert error">{{.Error}}</div>{{end}}
    {{if .Notice}}<div class="alert notice">{{.Notice}}</div>{{end}}

    <section class="stats">
      <div class="stat"><strong>{{.Stats.Subscribers}}</strong><span>Subscribers</span></div>
      <div class="stat"><strong>{{.Stats.Keywords}}</strong><span>Keywords</span></div>
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
        <h2>Keywords</h2>
        <form method="post" action="/keywords/add" class="form-row">
          <input name="phrase" placeholder="Phrase to monitor" required>
          <button type="submit">Add</button>
        </form>
        {{range .Keywords}}
          <div class="keyword">
            <code>#{{.ID}} {{.Phrase}}</code>
            <form method="post" action="/keywords/delete">
              <input type="hidden" name="id" value="{{.ID}}">
              <button class="delete" type="submit">Delete</button>
            </form>
          </div>
        {{else}}
          <p class="muted">No keywords yet.</p>
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
        <p class="muted">Scans previous messages for current keywords. Old matches are stored, not sent as Telegram alerts.</p>
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
