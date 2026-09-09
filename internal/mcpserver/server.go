package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

type Server struct {
	monitor   *monitor.Service
	media     *media.Service
	publicURL string
	token     string
	handler   http.Handler
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
	Chat     string `json:"chat" jsonschema:"Chat key returned by another Telegram tool, for example channel:123"`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum number of messages to return, from 1 to 100"`
	OffsetID int    `json:"offset_id,omitempty" jsonschema:"Return messages older than this message ID; omit for latest messages"`
}

type Options struct {
	Media     *media.Service
	PublicURL string
}

func New(service *monitor.Service, token string, options ...Options) *Server {
	server := &Server{monitor: service, token: strings.TrimSpace(token)}
	if len(options) > 0 {
		server.media = options[0].Media
		server.publicURL = strings.TrimRight(options[0].PublicURL, "/")
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
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "telegram_get_history",
		Description: "Read recent or paginated message history from one Telegram chat belonging to the logged-in account. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, server.getHistory)
	if server.media != nil {
		server.addMediaTools(mcpServer)
	}
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.Handle("/", streamable)
	mux.HandleFunc("/mcp/media/", server.downloadFile)
	server.handler = server.authenticate(mux)
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) listDialogs(ctx context.Context, _ *mcp.CallToolRequest, input listDialogsInput) (*mcp.CallToolResult, listDialogsOutput, error) {
	dialogs, err := s.monitor.ListDialogs(ctx, input.Query, input.Limit)
	return nil, listDialogsOutput{Dialogs: dialogs}, err
}

func (s *Server) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, input searchMessagesInput) (*mcp.CallToolResult, messagesOutput, error) {
	minDate, err := parseDate(input.MinDate)
	if err != nil {
		return nil, messagesOutput{}, err
	}
	maxDate, err := parseDate(input.MaxDate)
	if err != nil {
		return nil, messagesOutput{}, err
	}
	messages, err := s.monitor.SearchMessages(ctx, monitor.MessageSearchOptions{
		Chat: input.Chat, Query: input.Query, Limit: input.Limit,
		MinDate: minDate, MaxDate: maxDate, OffsetID: input.OffsetID,
	})
	return nil, messagesOutput{Messages: messages}, err
}

func (s *Server) getHistory(ctx context.Context, _ *mcp.CallToolRequest, input getHistoryInput) (*mcp.CallToolResult, messagesOutput, error) {
	messages, err := s.monitor.GetHistory(ctx, monitor.HistoryOptions{Chat: input.Chat, Limit: input.Limit, OffsetID: input.OffsetID})
	return nil, messagesOutput{Messages: messages}, err
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.token == "" || provided == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="telegram-bridge-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
