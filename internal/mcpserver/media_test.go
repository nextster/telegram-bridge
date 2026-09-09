package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/media"
)

type mcpMediaSource struct{}

func (mcpMediaSource) AccountID() int64 { return 1 }
func (mcpMediaSource) Attachment(_ context.Context, chat string, id int) (media.Attachment, error) {
	return media.Attachment{AccountID: 1, Chat: chat, MessageID: id, Kind: "voice", Size: 5, Duration: 1, Supported: true, Fingerprint: "stable", MIME: "audio/ogg", Date: time.Unix(1700000000, 0)}, nil
}
func (mcpMediaSource) Download(_ context.Context, _ media.Attachment, w io.Writer) error {
	_, err := io.WriteString(w, "audio")
	return err
}

func mediaHTTPServer(t *testing.T) (*httptest.Server, *media.Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := config.DefaultMediaConfig(path)
	cfg.Enabled = true
	cfg.APIKey = "fake-key-never-used"
	service, err := media.New(cfg, store, mcpMediaSource{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	server := httptest.NewServer(New(nil, "test-token", Options{Media: service}))
	t.Cleanup(server.Close)
	return server, service
}

func TestMediaDownloadsRequireBearerOnEveryRequest(t *testing.T) {
	server, service := mediaHTTPServer(t)
	d, err := service.Download(context.Background(), "channel:1", 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {"wrong", 401}, {"test-token", 200}} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/mcp/media/"+d.FileID, nil)
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("status=%d want=%d", response.StatusCode, tc.status)
		}
		if tc.status == 200 && (string(body) != "audio" || response.Header.Get("Cache-Control") != "private, no-store" || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;")) {
			t.Fatal("wrong private download response")
		}
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/mcp/media/not-a-file", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("invalid file identifier not rejected")
	}
	for _, token := range []string{"", "Bearer "} {
		r := httptest.NewRequest(http.MethodGet, "/mcp/media/"+d.FileID, nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		New(nil, "").ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("empty configured token allowed access")
		}
	}
}

func TestMediaMCPToolsSchemasAndExplicitStart(t *testing.T) {
	server, _ := mediaHTTPServer(t)
	transport := http.DefaultTransport
	clientHTTP := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.Header = r.Header.Clone()
		clone.Header.Set("Authorization", "Bearer test-token")
		return transport.RoundTrip(clone)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "media-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: clientHTTP, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 9 {
		t.Fatalf("got %d tools", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.Name == "telegram_transcribe_media" || tool.Name == "telegram_analyze_image" || tool.Name == "telegram_download_attachment" {
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal("wrong media side-effect annotation")
			}
		}
		if tool.Name == "telegram_analyze_image" || tool.Name == "telegram_transcribe_media" {
			raw, _ := json.Marshal(tool.InputSchema)
			var schema struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			}
			if json.Unmarshal(raw, &schema) != nil {
				t.Fatal("invalid schema")
			}
			found := false
			for _, key := range schema.Required {
				if key == "confirm_paid" {
					found = true
				}
			}
			if !found {
				t.Fatal("paid confirmation optional in schema")
			}
			if schema.Properties["url"] != nil || schema.Properties["path"] != nil {
				t.Fatal("arbitrary file input exposed")
			}
		}
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	args := map[string]any{"chat": "channel:1", "message_id": 7, "confirm_paid": false}
	if !call("telegram_transcribe_media", args).IsError {
		t.Fatal("implicit paid call accepted")
	}
	args["confirm_paid"] = true
	first := call("telegram_transcribe_media", args)
	if first.IsError {
		t.Fatalf("start failed: %+v", first)
	}
	data := first.StructuredContent.(map[string]any)
	id := data["job_id"].(string)
	second := call("telegram_transcribe_media", args).StructuredContent.(map[string]any)
	if second["job_id"] != id {
		t.Fatal("MCP duplicate not reused")
	}
	result := call("telegram_get_transcription", map[string]any{"chat": "channel:1", "message_id": 7, "job_id": id})
	if result.IsError || result.StructuredContent.(map[string]any)["status"] != "queued" {
		t.Fatal("job polling failed")
	}
	if !call("telegram_get_transcription", map[string]any{"chat": "channel:2", "message_id": 7, "job_id": id}).IsError {
		t.Fatal("result crossed chat boundary")
	}
	if !call("telegram_get_image_analysis", map[string]any{"chat": "channel:1", "message_id": 7, "job_id": id}).IsError {
		t.Fatal("transcript presented as image result")
	}
	if !call("telegram_get_attachment", map[string]any{"chat": "https://example.com", "message_id": 7}).IsError {
		t.Fatal("URL accepted")
	}
	if !call("telegram_download_attachment", map[string]any{"chat": "channel:1", "message_id": 7, "path": "/etc/passwd"}).IsError {
		t.Fatal("unknown path input accepted")
	}
}
