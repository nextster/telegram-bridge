package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

type Server struct {
	accounts            AccountResolver
	media               *media.Service
	publicURL           string
	verifyToken         TokenVerifier
	resourceMetadataURL string
	handler             http.Handler
}

// TokenVerifier resolves a bearer token to the Telegram user it belongs to. It
// returns auth.ErrInvalidToken for unknown, expired, or revoked tokens.
type TokenVerifier func(ctx context.Context, token string) (userID int64, expires time.Time, err error)

// AccountResolver returns the connected Telegram account of a user. Tools only
// ever act on the account of the authenticated caller.
type AccountResolver func(userID int64) (Monitor, error)

var errNoPrincipal = errors.New("request is not authenticated")

type Monitor interface {
	ListDialogs(context.Context, string, int) ([]monitor.TelegramDialog, error)
	SearchMessages(context.Context, monitor.MessageSearchOptions) ([]monitor.TelegramMessage, error)
	GetHistoryPage(context.Context, monitor.HistoryOptions) (monitor.HistoryPage, error)
}

type listDialogsInput struct {
	Query string `json:"query,omitempty" jsonschema:"Optional case-insensitive title, username, or chat-key filter"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of dialogs to return, from 1 to 100"`
}

type listDialogsOutput struct {
	Dialogs []monitor.TelegramDialog `json:"dialogs"`
}

type searchMessagesInput struct {
	Query    string `json:"query" jsonschema:"Text to search for"`
	Chat     string `json:"chat,omitempty" jsonschema:"Optional chat key returned by another Telegram tool, for example channel:123"`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum number of messages to return, from 1 to 100"`
	MinDate  string `json:"min_date,omitempty" jsonschema:"Optional inclusive lower date bound in RFC3339 or YYYY-MM-DD format"`
	MaxDate  string `json:"max_date,omitempty" jsonschema:"Optional exclusive upper date bound in RFC3339 or YYYY-MM-DD format"`
	OffsetID int    `json:"offset_id,omitempty" jsonschema:"Message ID offset for pagination"`
}

type messagesOutput struct {
	Messages []monitor.TelegramMessage `json:"messages"`
}

type getHistoryInput struct {
	Chat         string        `json:"chat" jsonschema:"Chat key returned by another Telegram tool, for example channel:123"`
	Limit        int           `json:"limit,omitempty" jsonschema:"Maximum number of messages to return, from 1 to 100"`
	OffsetID     int           `json:"offset_id,omitempty" jsonschema:"Return messages older than this message ID; omit for latest messages"`
	MinDate      string        `json:"min_date,omitempty" jsonschema:"Inclusive date lower bound in RFC3339 or YYYY-MM-DD; required for process_media"`
	MaxDate      string        `json:"max_date,omitempty" jsonschema:"Exclusive upper date bound; reuse the returned value when resuming a paid history page"`
	ProcessMedia bool          `json:"process_media,omitempty" jsonschema:"Explicitly recognize all supported media in the returned page and wait; requires confirm_paid=true. Default false: no cloud processing"`
	ConfirmPaid  bool          `json:"confirm_paid,omitempty" jsonschema:"Must be true to authorize paid recognition in this date-bounded history page"`
	WaitSeconds  *int          `json:"wait_seconds,omitempty" jsonschema:"Processing wait timeout, 0..480 seconds; default 480. Pending jobs continue after timeout"`
	Audio        media.Options `json:"audio,omitempty" jsonschema:"Optional speech model and bounded spelling/language hints; part of the cache identity"`
	Image        media.Options `json:"image,omitempty" jsonschema:"Optional image model and description_language; part of cache identity"`
}

type historyOutput struct {
	Messages     []monitor.TelegramMessage `json:"messages"`
	HasMore      bool                      `json:"has_more"`
	NextOffsetID int                       `json:"next_offset_id,omitempty"`
	MinDate      string                    `json:"min_date,omitempty"`
	MaxDate      string                    `json:"max_date,omitempty"`
	Media        *media.Batch              `json:"media,omitempty"`
	SkippedMedia []skippedMedia            `json:"skipped_media,omitempty" jsonschema:"Attachments not eligible for recognition; never submitted or silently counted as successes"`
}

type skippedMedia struct {
	Chat      string `json:"chat"`
	MessageID int    `json:"message_id"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
}

type Options struct {
	Accounts    AccountResolver
	VerifyToken TokenVerifier
	Media       *media.Service
	PublicURL   string
	// ResourceMetadataURL is advertised in 401 challenges when OAuth is enabled.
	ResourceMetadataURL string
}

func New(options Options) *Server {
	server := &Server{
		accounts:            options.Accounts,
		media:               options.Media,
		publicURL:           strings.TrimRight(options.PublicURL, "/"),
		verifyToken:         options.VerifyToken,
		resourceMetadataURL: options.ResourceMetadataURL,
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "telegram-bridge", Version: "1.0.0"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "telegram_list_dialogs",
		Description: "List recent Telegram dialogs for the logged-in account, optionally filtering by chat title or username. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, server.listDialogs)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "telegram_search_messages",
		Description: "Search messages across all dialogs or within one Telegram chat belonging to the logged-in account. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, server.searchMessages)
	historyAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: true}
	if server.media != nil {
		no := false
		historyAnnotations = &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no}
	}
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "telegram_get_history",
		Description: "Read date-bounded or paginated history. Free by default. With process_media=true and confirm_paid=true, queue all supported media in the returned page and wait for their cached results. has_more means older pages remain; pending/failed media is never reported as complete.",
		Annotations: historyAnnotations,
	}, server.getHistory)
	if server.media != nil {
		server.addMediaTools(mcpServer)
	}
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.Handle("/", streamable)
	mux.HandleFunc("/mcp/media/", server.downloadFile)
	requireToken := auth.RequireBearerToken(server.verifyBearer, &auth.RequireBearerTokenOptions{ResourceMetadataURL: server.resourceMetadataURL})
	server.handler = challengeRealm(requireToken(mux))
	return server
}

func (s *Server) verifyBearer(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if s.verifyToken == nil {
		return nil, auth.ErrInvalidToken
	}
	userID, expires, err := s.verifyToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if userID <= 0 || expires.IsZero() {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: strconv.FormatInt(userID, 10), Expiration: expires}, nil
}

// principal returns the Telegram user of an authenticated tool call.
func principal(req *mcp.CallToolRequest) (int64, error) {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil {
		return 0, errNoPrincipal
	}
	return parsePrincipal(req.Extra.TokenInfo)
}

func parsePrincipal(info *auth.TokenInfo) (int64, error) {
	if info == nil {
		return 0, errNoPrincipal
	}
	userID, err := strconv.ParseInt(info.UserID, 10, 64)
	if err != nil || userID <= 0 {
		return 0, errNoPrincipal
	}
	return userID, nil
}

func (s *Server) account(req *mcp.CallToolRequest) (Monitor, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, err
	}
	if s.accounts == nil {
		return nil, media.Fail("telegram_unavailable")
	}
	account, err := s.accounts(userID)
	if err != nil || account == nil {
		return nil, errors.New("your Telegram account is not connected; send /login to the bot")
	}
	return account, nil
}

// challengeRealm names the realm in 401 responses that carry no OAuth
// resource metadata challenge.
func challengeRealm(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&realmWriter{ResponseWriter: w}, r)
	})
}

type realmWriter struct {
	http.ResponseWriter
}

func (w *realmWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="telegram-bridge-mcp"`)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *realmWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *realmWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) listDialogs(ctx context.Context, req *mcp.CallToolRequest, input listDialogsInput) (*mcp.CallToolResult, listDialogsOutput, error) {
	account, err := s.account(req)
	if err != nil {
		return nil, listDialogsOutput{}, err
	}
	dialogs, err := account.ListDialogs(ctx, input.Query, input.Limit)
	return nil, listDialogsOutput{Dialogs: dialogs}, err
}

func (s *Server) searchMessages(ctx context.Context, req *mcp.CallToolRequest, input searchMessagesInput) (*mcp.CallToolResult, messagesOutput, error) {
	account, err := s.account(req)
	if err != nil {
		return nil, messagesOutput{}, err
	}
	minDate, err := parseDate(input.MinDate)
	if err != nil {
		return nil, messagesOutput{}, err
	}
	maxDate, err := parseDate(input.MaxDate)
	if err != nil {
		return nil, messagesOutput{}, err
	}
	messages, err := account.SearchMessages(ctx, monitor.MessageSearchOptions{
		Chat: input.Chat, Query: input.Query, Limit: input.Limit,
		MinDate: minDate, MaxDate: maxDate, OffsetID: input.OffsetID,
	})
	return nil, messagesOutput{Messages: messages}, err
}

func parseDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("date must use RFC3339 or YYYY-MM-DD format")
}
