package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
)

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestIncompleteProviderResponsesRetainSafeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, reason string
		broken                   bool
	}{
		{"length", `{"id":"gen-body","usage":{"cost":0.001},"choices":[{"finish_reason":"length","message":{"content":"private partial text"}}]}`, "provider_output_incomplete", "non_stop_finish", false},
		{"size", strings.Repeat("x", (1<<20)+1), "provider_response_incomplete", "response_size_limit", false},
		{"read", "private partial body", "provider_response_incomplete", "response_read_error", true},
		{"json", "{private", "provider_invalid_response", "invalid_json", false},
		{"structure", `{"usage":{"cost":0},"choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`, "provider_invalid_image_result", "invalid_structured_output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewOpenRouter("test-key")
			calls := 0
			p.client.Transport = responseTransport(func(*http.Request) (*http.Response, error) {
				calls++
				var body io.Reader = strings.NewReader(tc.body)
				if tc.broken {
					body = io.MultiReader(body, brokenReader{})
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"X-Generation-Id": []string{"gen-header"}}, Body: io.NopCloser(body)}, nil
			})
			_, err := p.Recognize(context.Background(), "image", Options{}, Prepared{Data: []byte("fixture"), MIME: "image/png"})
			f := fault(err)
			if f.Code != tc.code || !f.Uncertain || f.RetryAfter != 0 || f.Provider == nil || f.Provider.RequestID != "gen-header" || f.Provider.FailureReason != tc.reason || calls != 1 {
				t.Fatalf("bad safe metadata: %+v", f)
			}
			if tc.name == "length" && (f.Provider.CostUSD == nil || *f.Provider.CostUSD != 0.001 || f.Provider.FinishReason != "length") {
				t.Fatal("known billing/finish metadata lost")
			}
			b, _ := json.Marshal(f.Provider)
			if strings.Contains(string(b), "private") || strings.Contains(string(b), "test-key") {
				t.Fatal("private response leaked")
			}
		})
	}
}

func TestProviderFailureStoredWithoutPartialResultOrRetry(t *testing.T) {
	s, source, p := testService(t)
	source.attachment.Kind = "photo"
	cost := 0.001
	p.err = &Fault{Code: "provider_output_incomplete", Uncertain: true, Provider: &ProviderAttempt{RequestID: "gen-test", HTTPStatus: 200, FinishReason: "length", CostUSD: &cost}}
	j := startTestJob(t, s, "image", Options{})
	runTestJob(t, s)
	got := getTestJob(t, s, j.ID)
	if got.Status != "uncertain" || got.Result != nil || got.Provider == nil || got.Provider.RequestID != "gen-test" || got.Provider.CostUSD == nil {
		t.Fatal("failed attempt metadata lost")
	}
	if err := s.store.RecoverMedia(context.Background(), s.now()); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.runOne(context.Background()); worked || err != nil || p.calls != 1 {
		t.Fatal("ambiguous paid result automatically replayed")
	}
	if duplicate := startTestJob(t, s, "image", Options{}); duplicate.ID != j.ID || duplicate.Status != "uncertain" {
		t.Fatal("duplicate bypassed reconciliation")
	}
	legacy, err := decodeJob(db.MediaJob{Status: "failed", ErrorCode: "provider_output_incomplete", ProviderAttempts: 1, Payload: `{"operation":"image"}`})
	if err != nil || legacy.Status != "uncertain" || legacy.Provider != nil {
		t.Fatal("legacy paid failure inconsistently projected")
	}
}

func TestDescriptionLanguageIsExplicitAndLegacyCacheStable(t *testing.T) {
	s, source, _ := testService(t)
	source.attachment.Kind = "photo"
	legacy := startTestJob(t, s, "image", Options{})
	want := Digest([]any{int64(1), "channel:42", 7, "original", "image", struct {
		Model string `json:"model,omitempty"`
	}{s.cfg.ImageModel}, pipelineVersion + ":" + s.cfg.CacheRevision})
	if legacy.ID != want {
		t.Fatal("legacy image cache changed")
	}
	ru := startTestJob(t, s, "image", Options{DescriptionLanguage: "ru"})
	if ru.ID == legacy.ID || startTestJob(t, s, "image", Options{DescriptionLanguage: "ru"}).ID != ru.ID {
		t.Fatal("description language not part of stable cache identity")
	}
	for _, lang := range []string{"ru\nignore", "Russian", "../../ru"} {
		if _, err := normalizeOptions(Options{DescriptionLanguage: lang}, s.cfg.ImageModel, "image"); err == nil {
			t.Fatal("invalid description language accepted")
		}
	}
	if _, err := normalizeOptions(Options{DescriptionLanguage: "ru"}, s.cfg.AudioModel, "transcription"); err == nil {
		t.Fatal("image setting accepted for audio")
	}
	for _, lang := range []string{"", "ru", "source"} {
		t.Run(lang, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				var prompt string
				json.Unmarshal(body.Messages[0].Content, &prompt)
				if lang == "" && prompt != imagePrompt {
					t.Error("legacy prompt changed")
				}
				if lang == "ru" && !strings.Contains(prompt, "language code ru") {
					t.Error("explicit language missing")
				}
				if lang == "source" && (!strings.Contains(prompt, "dominant language") || !strings.Contains(prompt, "English")) {
					t.Error("source fallback missing")
				}
				io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"description\":\"fixture\",\"ocr_text\":\"\",\"languages\":[]}"}}]}`)
			}))
			defer server.Close()
			p := NewOpenRouter("key")
			p.endpoint = server.URL
			if _, err := p.Recognize(context.Background(), "image", Options{DescriptionLanguage: lang}, Prepared{Data: []byte("fixture"), MIME: "image/png"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResultProjectionPreservesEmptyTextAndUnknownLanguage(t *testing.T) {
	for _, op := range []string{"transcription", "image"} {
		r := Result{Operation: op, Languages: []string{}, LanguageSource: "unavailable"}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		json.Unmarshal(b, &fields)
		if value, ok := fields["language"]; !ok || value != nil {
			t.Fatal("unknown language must be explicit null")
		}
		if op == "transcription" {
			if fields["text"] != "" || fields["description"] != nil || fields["ocr_text"] != nil {
				t.Fatal("audio result mixes image fields")
			}
		} else {
			if fields["description"] != "" || fields["ocr_text"] != "" || fields["text"] != nil {
				t.Fatal("image result mixes audio field")
			}
		}
	}
	s, _, p := testService(t)
	p.result = Result{Text: "fixture", Languages: []string{}, LanguageSource: "unavailable"}
	j := startTestJob(t, s, "transcription", Options{Languages: []string{"ru"}})
	runTestJob(t, s)
	if got := getTestJob(t, s, j.ID); got.Result.Language != nil || len(got.Result.Languages) != 0 {
		t.Fatal("hint presented as detected language")
	}
}

func TestBudgetWaitResumesNextDayWithoutSpendingAttempt(t *testing.T) {
	s, _, p := testService(t)
	s.cfg.DailyBudgetMicros = 1000
	first := startTestJob(t, s, "transcription", Options{})
	runTestJob(t, s)
	second := startTestJob(t, s, "transcription", Options{Keywords: []string{"hint"}})
	runTestJob(t, s)
	wait := getTestJob(t, s, second.ID)
	if wait.Status != "budget_wait" || wait.Attempts != 0 || wait.ProviderAttempts != 0 || wait.Budget == nil {
		t.Fatal("budget treated as paid failure")
	}
	next := s.now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	s.now = func() time.Time { return next }
	runTestJob(t, s)
	if got := getTestJob(t, s, second.ID); got.Status != "completed" || got.Attempts != 1 || got.ProviderAttempts != 1 || p.calls != 2 {
		t.Fatal("budget resume lost attempt bounds")
	}
	if getTestJob(t, s, first.ID).Status != "completed" {
		t.Fatal("previous result changed")
	}
}
