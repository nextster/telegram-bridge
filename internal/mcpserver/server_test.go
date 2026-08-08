package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestServerRejectsMissingToken(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	New(nil, "secret").ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("WWW-Authenticate header is missing")
	}
}

func TestMCPHandshakeAndToolsList(t *testing.T) {
	httpServer := httptest.NewServer(New(nil, "secret"))
	defer httpServer.Close()

	baseTransport := http.DefaultTransport
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.Header.Set("Authorization", "Bearer secret")
		return baseTransport.RoundTrip(clone)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "tg-radar-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(result.Tools) != 3 {
		t.Fatalf("tool count = %d, want 3", len(result.Tools))
	}
	foundHistory := false
	for _, tool := range result.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %q is not marked read-only", tool.Name)
		}
		if tool.Name != "telegram_get_history" {
			continue
		}
		foundHistory = true
		rawSchema, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("marshal history output schema: %v", err)
		}
		var schema struct {
			Properties map[string]struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(rawSchema, &schema); err != nil {
			t.Fatalf("decode history output schema: %v", err)
		}
		messageProperties := schema.Properties["messages"].Items.Properties
		for _, name := range []string{"reply_to_id", "topic_id", "media_kind", "edited_at"} {
			if _, ok := messageProperties[name]; !ok {
				t.Errorf("history output schema is missing message property %q: %s", name, rawSchema)
			}
		}
	}
	if !foundHistory {
		t.Fatal("telegram_get_history tool is missing")
	}
}

func TestParseDate(t *testing.T) {
	for _, value := range []string{"", "2026-07-18", "2026-07-18T12:30:00Z"} {
		if _, err := parseDate(value); err != nil {
			t.Fatalf("parseDate(%q): %v", value, err)
		}
	}
	if _, err := parseDate("18 July"); err == nil {
		t.Fatal("parseDate accepted invalid input")
	}
}
