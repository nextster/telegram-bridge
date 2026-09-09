package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenRouterTranscriptionContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong endpoint or auth")
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("undocumented idempotency enabled")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		if body["model"] != "openai/gpt-transcribe" || body["response_format"] != "json" || body["language"] != nil || body["prompt"] != nil {
			t.Error("wrong STT fields")
		}
		audio := body["input_audio"].(map[string]any)
		data, _ := base64.StdEncoding.DecodeString(audio["data"].(string))
		if string(data) != "mp3 bytes" || audio["format"] != "mp3" {
			t.Error("wrong encoded audio")
		}
		options := body["provider"].(map[string]any)["options"].(map[string]any)["openai"].(map[string]any)
		if options["prompt"] != transcriptionPrompt || options["keywords"].([]any)[0] != "Realize" || options["languages"].([]any)[0] != "ru" {
			t.Error("provider hints lost")
		}
		w.Header().Set("X-Generation-Id", "gen-test-1")
		io.WriteString(w, `{"text":"Exact words.\nSecond line.","languages":[{"code":"ru"}],"usage":{"cost":0.001}}`)
	}))
	defer server.Close()
	p := NewOpenRouter("test-key")
	p.endpoint = server.URL
	result, err := p.Recognize(context.Background(), "transcription", Options{Model: "openai/gpt-transcribe", Keywords: []string{"Realize"}, Languages: []string{"ru"}}, Prepared{Data: []byte("mp3 bytes")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Exact words.\nSecond line." || len(result.Languages) != 1 || result.LanguageSource != "provider" || result.ProviderRequestID != "gen-test-1" || result.CostUSD == nil {
		t.Fatal("result metadata lost")
	}
}

func TestOpenRouterImageSeparatesDescriptionAndText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Error("wrong image endpoint")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["max_tokens"] != float64(8192) || body["model"] != "openai/gpt-4.1-mini" || body["stream"] != false {
			t.Error("unbounded image request")
		}
		provider := body["provider"].(map[string]any)
		if provider["require_parameters"] != true || provider["allow_fallbacks"] != false {
			t.Error("wrong routing")
		}
		format := body["response_format"].(map[string]any)
		if format["type"] != "json_schema" {
			t.Error("missing structured output")
		}
		messages := body["messages"].([]any)
		content := messages[1].(map[string]any)["content"].([]any)
		url := content[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(url, "data:image/png;base64,") {
			t.Error("image not embedded privately")
		}
		io.WriteString(w, `{"id":"gen-image","choices":[{"finish_reason":"stop","message":{"content":"{\"description\":\"A sign\",\"ocr_text\":\"Hello\\nWorld\",\"languages\":[\"en\"]}"}}],"usage":{"cost":0.002}}`)
	}))
	defer server.Close()
	p := NewOpenRouter("test-key")
	p.endpoint = server.URL
	result, err := p.Recognize(context.Background(), "image", Options{Model: "openai/gpt-4.1-mini"}, Prepared{Data: []byte("png bytes"), MIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Description != "A sign" || result.OCRText != "Hello\nWorld" || result.Text != "" {
		t.Fatal("image outputs mixed")
	}
}

func TestOpenRouterErrorsNeverExposeBodiesOrRetryUnknownCharges(t *testing.T) {
	for _, tc := range []struct {
		status           int
		code             string
		retry, uncertain bool
	}{
		{400, "openrouter_request_rejected", false, false}, {401, "openrouter_auth_error", false, false}, {403, "openrouter_auth_error", false, false}, {402, "openrouter_credit_limit", false, false},
		{413, "openrouter_request_rejected", false, false}, {429, "openrouter_rate_limited", true, false}, {500, "provider_outcome_unknown", false, true}, {502, "provider_outcome_unknown", false, true},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(tc.status)
				io.WriteString(w, "secret audio and transcript test-key")
			}))
			defer server.Close()
			p := NewOpenRouter("test-key")
			p.endpoint = server.URL
			_, err := p.Recognize(context.Background(), "transcription", Options{}, Prepared{Data: []byte("audio")})
			f := fault(err)
			if f.Code != tc.code || f.Uncertain != tc.uncertain || (f.RetryAfter > 0) != tc.retry || calls != 1 {
				t.Fatalf("unexpected fault: %+v calls=%d", f, calls)
			}
			if tc.retry && f.RetryAfter < 120*time.Second {
				t.Fatal("Retry-After ignored")
			}
		})
	}
}

func TestOpenRouterMalformedTruncatedAndMissingLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, body, operation string
		wantErr               bool
	}{
		{"missing_text", `{"usage":{}}`, "transcription", true},
		{"missing_language", `{"text":"verbatim"}`, "transcription", false},
		{"malformed", `{"text":`, "transcription", true},
		{"embedded_error", `{"error":{"message":"private details"}}`, "transcription", true},
		{"truncated_image", `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`, "image", true},
		{"refusal", `{"choices":[{"finish_reason":"stop","message":{"refusal":"no"}}]}`, "image", true},
		{"missing_ocr", `{"choices":[{"finish_reason":"stop","message":{"content":"{\"description\":\"sign\",\"languages\":[]}"}}]}`, "image", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
			defer server.Close()
			p := NewOpenRouter("key")
			p.endpoint = server.URL
			result, err := p.Recognize(context.Background(), tc.operation, Options{Languages: []string{"ru"}}, Prepared{Data: []byte("test")})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if !tc.wantErr && (len(result.Languages) != 0 || result.LanguageSource != "unavailable") {
				t.Fatal("invented detected language from hint")
			}
		})
	}
}

func TestOpenRouterDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	p := NewOpenRouter("key")
	p.endpoint = server.URL
	_, err := p.Recognize(context.Background(), "transcription", Options{}, Prepared{Data: []byte("test")})
	if err == nil || calls != 0 {
		t.Fatal("redirect leaked media")
	}
}
