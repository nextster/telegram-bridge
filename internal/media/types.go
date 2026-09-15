package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nextster/telegram-bridge/internal/db"
)

const pipelineVersion = "openrouter-media-v1-mp3-48k"
const maxAttempts = 3

type Attachment struct {
	AccountID   int64     `json:"account_id"`
	Chat        string    `json:"chat"`
	MessageID   int       `json:"message_id"`
	Author      string    `json:"author"`
	Date        time.Time `json:"date"`
	ReplyToID   int       `json:"reply_to_id,omitempty"`
	ReplyToChat string    `json:"reply_to_chat,omitempty"`
	TopicID     int       `json:"topic_id,omitempty"`
	MessageURL  string    `json:"message_url,omitempty"`
	Kind        string    `json:"kind"`
	MIME        string    `json:"mime_type"`
	Size        int64     `json:"size_bytes"`
	Duration    float64   `json:"duration_seconds"`
	Supported   bool      `json:"supported"`
	Fingerprint string    `json:"fingerprint"`
}

// Source reads Telegram media through one account's own session. Every call
// names the account; an attachment of another account is never returned.
type Source interface {
	Attachment(ctx context.Context, accountID int64, chat string, id int) (Attachment, error)
	Download(ctx context.Context, accountID int64, expected Attachment, output io.Writer) error
	LiveAccounts() []int64
}

type Options struct {
	Model               string   `json:"model,omitempty"`
	Keywords            []string `json:"keywords,omitempty"`
	Languages           []string `json:"languages,omitempty"`
	DescriptionLanguage string   `json:"description_language,omitempty" jsonschema:"Image only: ISO language code or source (dominant visible-text language, English if unknown); omitted preserves legacy prompt/cache"`
}

type Result struct {
	Operation         string   `json:"-"`
	Text              string   `json:"text,omitempty"`
	Description       string   `json:"description,omitempty"`
	OCRText           string   `json:"ocr_text,omitempty"`
	Language          *string  `json:"language" jsonschema:"Single provider-detected language; null when absent, unknown, or multilingual; never inferred from hints"`
	Languages         []string `json:"languages"`
	LanguageSource    string   `json:"language_source"`
	ProviderRequestID string   `json:"provider_request_id,omitempty"`
	CostUSD           *float64 `json:"cost_usd,omitempty"`
}

// Emit empty valid transcripts/OCR, but not fields belonging to another media
// operation. The Go value remains shared for storage and provider plumbing.
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result
	b, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	if r.Operation == "image" {
		delete(fields, "text")
		fields["description"], _ = json.Marshal(r.Description)
		fields["ocr_text"], _ = json.Marshal(r.OCRText)
	} else if r.Operation == "transcription" {
		delete(fields, "description")
		delete(fields, "ocr_text")
		fields["text"], _ = json.Marshal(r.Text)
	}
	return json.Marshal(fields)
}

type ProviderAttempt struct {
	RequestID     string   `json:"request_id,omitempty"`
	HTTPStatus    int      `json:"http_status,omitempty"`
	FinishReason  string   `json:"finish_reason,omitempty"`
	ResponseBytes int      `json:"response_bytes,omitempty"`
	FailureReason string   `json:"failure_reason,omitempty"`
	CostUSD       *float64 `json:"cost_usd,omitempty"`
}

type failureMeta struct {
	Provider *ProviderAttempt `json:"provider,omitempty"`
	Budget   *db.MediaBudget  `json:"budget,omitempty"`
}

type Job struct {
	ID               string           `json:"job_id"`
	Operation        string           `json:"operation"`
	Status           string           `json:"status"`
	ErrorCode        string           `json:"error_code,omitempty"`
	Attempts         int              `json:"attempts"`
	ProviderAttempts int              `json:"provider_attempts"`
	NextAttemptAt    *time.Time       `json:"next_attempt_at,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
	Source           Attachment       `json:"source"`
	Settings         Options          `json:"settings"`
	SHA256           string           `json:"sha256,omitempty"`
	Duration         float64          `json:"duration_seconds"`
	Result           *Result          `json:"result,omitempty"`
	Provider         *ProviderAttempt `json:"provider,omitempty" jsonschema:"Allowlisted metadata from an unusable provider response; not a recognition result"`
	Budget           *db.MediaBudget  `json:"budget,omitempty"`
}

// Faults expose only stable codes, never upstream bodies or private media.
type Fault struct {
	Code       string
	RetryAfter time.Duration
	Uncertain  bool
	Provider   *ProviderAttempt
}

func (e *Fault) Error() string                     { return e.Code }
func Fail(code string) error                       { return &Fault{Code: code} }
func Retry(code string, delay time.Duration) error { return &Fault{Code: code, RetryAfter: delay} }

var idPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var chatPattern = regexp.MustCompile(`^(user|chat|channel):[1-9][0-9]{0,18}$`)
var languagePattern = regexp.MustCompile(`^([a-z]{2,3}|zh-(cn|tw|hk))$`)

func ValidateReference(chat string, messageID int) error {
	if !chatPattern.MatchString(chat) || messageID <= 0 || messageID > 2147483647 {
		return Fail("invalid_message_reference")
	}
	return nil
}

func Digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeOptions(o Options, model, operation string) (Options, error) {
	// Callers may share a batch's hints between concurrent requests.
	o.Keywords = append([]string(nil), o.Keywords...)
	o.Languages = append([]string(nil), o.Languages...)
	if o.Model == "" {
		o.Model = model
	}
	if o.Model != model {
		return o, Fail("unsupported_model")
	}
	if operation == "image" && (len(o.Keywords) > 0 || len(o.Languages) > 0) {
		return o, Fail("image_hints_not_supported")
	}
	if o.DescriptionLanguage != "" && (operation != "image" || (o.DescriptionLanguage != "source" && !languagePattern.MatchString(o.DescriptionLanguage))) {
		return o, Fail("invalid_description_language")
	}
	if len(o.Keywords) > 16 || len(o.Languages) > 4 {
		return o, Fail("too_many_hints")
	}
	total := 0
	for i, term := range o.Keywords {
		if strings.ContainsAny(term, "<>\r\n") || strings.IndexFunc(term, unicode.IsControl) >= 0 {
			return o, Fail("invalid_keyword")
		}
		term = strings.TrimSpace(term)
		if term == "" || len(term) > 80 {
			return o, Fail("invalid_keyword")
		}
		o.Keywords[i] = term
		total += len(term)
	}
	if total > 512 {
		return o, Fail("hints_too_long")
	}
	for _, code := range o.Languages {
		if !languagePattern.MatchString(code) {
			return o, Fail("invalid_language")
		}
	}
	o.Keywords = sortedUnique(o.Keywords)
	o.Languages = sortedUnique(o.Languages)
	return o, nil
}

func sortedUnique(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	out := make([]string, 0, len(result))
	for _, v := range result {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func fault(err error) *Fault {
	var f *Fault
	if errors.As(err, &f) {
		return f
	}
	return &Fault{Code: "internal_error"}
}
