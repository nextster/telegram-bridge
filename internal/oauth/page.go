package oauth

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
)

type approvalPage struct {
	ClientName    string
	SignInLink    string
	SignedIn      bool
	Code          string
	ApprovalLink  string
	RequestID     string
	BrowserSecret string
	Nonce         string
	Error         string
}

type pageRenderer struct {
	template *template.Template
}

func newPageRenderer() pageRenderer {
	return pageRenderer{template: template.Must(template.New("oauth").Parse(pageTemplate))}
}

func (s *Server) renderApproval(w http.ResponseWriter, page approvalPage) {
	page.Nonce = randomToken(16)
	s.writePage(w, http.StatusOK, page)
}

func (s *Server) renderError(w http.ResponseWriter, status int, message string) {
	s.writePage(w, status, approvalPage{Error: message})
}

func (s *Server) writePage(w http.ResponseWriter, status int, page approvalPage) {
	var body bytes.Buffer
	if err := s.pageTemplate.template.Execute(&body, page); err != nil {
		log.Printf("render oauth page failed: %v", err)
		http.Error(w, "could not render authorization page", http.StatusInternalServerError)
		return
	}
	script := "'none'"
	if page.Nonce != "" {
		script = "'nonce-" + page.Nonce + "'"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src "+script+"; connect-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

const pageTemplate = `<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <title>Подключение к Telegram Bridge</title>
  <style>
    :root { color-scheme: light dark; --bg: #f6f7f9; --panel: #fff; --text: #17202a; --muted: #637083; --line: #d9dee7; --accent: #0f766e; --danger: #b42318; }
    @media (prefers-color-scheme: dark) { :root { --bg: #111418; --panel: #1a1f25; --text: #e7eaee; --muted: #9aa5b1; --line: #2c333b; --accent: #2dd4bf; --danger: #f97066; } }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--text); font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    main { width: min(440px, calc(100% - 32px)); margin: 12vh auto 0; }
    .panel { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 22px; }
    h1 { margin: 0 0 6px; font-size: 21px; line-height: 1.2; }
    p { margin: 10px 0 0; }
    .muted { color: var(--muted); font-size: 13px; }
    .code { margin: 18px 0 4px; font-size: 56px; font-weight: 700; letter-spacing: .08em; text-align: center; font-variant-numeric: tabular-nums; }
    .status { margin-top: 16px; padding: 10px 12px; border-radius: 8px; border: 1px solid var(--line); }
    .status.ok { border-color: var(--accent); color: var(--accent); }
    .status.bad { border-color: var(--danger); color: var(--danger); }
    ol { margin: 14px 0 0; padding-left: 20px; }
    li { margin-top: 6px; }
    .button { display: block; margin: 14px 0 0; padding: 12px 16px; border-radius: 8px; background: var(--accent); color: #fff; font-weight: 600; text-align: center; text-decoration: none; }
    @media (prefers-color-scheme: dark) { .button { color: #0b1f1d; } }
  </style>
</head>
<body>
<main>
  <div class="panel">
  {{if .Error}}
    <h1>Подключение не удалось</h1>
    <p class="status bad">{{.Error}}</p>
  {{else if .SignInLink}}
    <h1>Подключение к Telegram Bridge</h1>
    <p><b>{{.ClientName}}</b> запрашивает доступ к вашему Telegram через MCP.</p>
    <p>Сначала войдите через Telegram в этом браузере. Доступ получит только аккаунт, под которым вы войдёте, и только после подтверждения в боте.</p>
    <a class="button" href="{{.SignInLink}}">Войти через Telegram</a>
    <p class="muted">Если эту ссылку вам прислал кто-то другой, закройте страницу: так пытаются получить доступ к чужой переписке.</p>
  {{else}}
    <h1>Подключение к Telegram Bridge</h1>
    <p><b>{{.ClientName}}</b> запрашивает доступ к вашему Telegram через MCP.</p>
    <p>Бот прислал вам запрос. Выберите в нём это число:</p>
    <div class="code" aria-label="Код подтверждения">{{.Code}}</div>
    <a class="button" href="{{.ApprovalLink}}" target="_blank" rel="noopener noreferrer">Открыть бота</a>
    <p id="status" class="status" role="status">Ждём подтверждения в Telegram…</p>
    <p class="muted">Если сообщение от бота не пришло, откройте бота кнопкой выше. Сначала подключите в нём свой Telegram командой /login.</p>
    <script nonce="{{.Nonce}}">
      (() => {
        const status = document.getElementById('status');
        const body = JSON.stringify({request: {{.RequestID}}, secret: {{.BrowserSecret}}});
        const show = (text, kind) => { status.textContent = text; status.className = 'status ' + (kind || ''); };
        const poll = async () => {
          let result;
          try {
            const response = await fetch('/oauth/authorize/status', {method: 'POST', headers: {'Content-Type': 'application/json'}, body, cache: 'no-store'});
            result = await response.json();
          } catch (_) {
            setTimeout(poll, 3000);
            return;
          }
          switch (result.status) {
            case 'pending':
              setTimeout(poll, 1500);
              return;
            case 'approved':
              show('Доступ разрешён. Возвращаемся в приложение…', 'ok');
              window.location.replace(result.redirect);
              return;
            case 'denied':
              show('Доступ отклонён.', 'bad');
              window.location.replace(result.redirect);
              return;
            case 'done':
              show('Подключение уже завершено. Эту страницу можно закрыть.', 'ok');
              return;
            default:
              show('Запрос устарел. Запустите подключение заново.', 'bad');
          }
        };
        poll();
      })();
    </script>
  {{end}}
  </div>
</main>
</body>
</html>`
