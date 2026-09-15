package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
)

type Processor interface {
	Prepare(context.Context, string, string, Attachment, int) (Prepared, error)
}
type Provider interface {
	Recognize(context.Context, string, Options, Prepared) (Result, error)
}
type Prepared struct {
	Data     []byte
	MIME     string
	Duration float64
}

type Service struct {
	cfg       config.MediaConfig
	store     *db.Store
	source    Source
	processor Processor
	provider  Provider
	root      *os.Root
	files     chan struct{}
	running   atomic.Bool
	now       func() time.Time
}

type jobPayload struct {
	Operation string     `json:"operation"`
	Source    Attachment `json:"source"`
	Settings  Options    `json:"settings"`
	Revision  string     `json:"revision"`
	SHA256    string     `json:"sha256"`
	Duration  float64    `json:"duration_seconds"`
}

func New(cfg config.MediaConfig, store *db.Store, source Source) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || source == nil {
		return nil, Fail("media_dependencies_missing")
	}
	if err := os.MkdirAll(cfg.Directory, 0700); err != nil {
		return nil, Fail("media_directory_unavailable")
	}
	if err := os.Chmod(cfg.Directory, 0700); err != nil {
		return nil, Fail("media_directory_unavailable")
	}
	root, err := os.OpenRoot(cfg.Directory)
	if err != nil {
		return nil, Fail("media_directory_unavailable")
	}
	s := &Service{cfg: cfg, store: store, source: source, root: root, files: make(chan struct{}, 1), now: time.Now, processor: FFmpeg{}, provider: NewOpenRouter(cfg.APIKey)}
	if err := s.cleanup(context.Background(), true); err != nil {
		root.Close()
		return nil, err
	}
	if err := s.store.RecoverMedia(context.Background(), s.now()); err != nil {
		root.Close()
		return nil, err
	}
	if err := s.store.ReconcileMediaCosts(context.Background()); err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}
func (s *Service) Close() error { return s.root.Close() }

func (s *Service) Metadata(ctx context.Context, accountID int64, chat string, id int) (Attachment, error) {
	if accountID <= 0 {
		return Attachment{}, Fail("telegram_account_unavailable")
	}
	if err := ValidateReference(chat, id); err != nil {
		return Attachment{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a, err := s.source.Attachment(ctx, accountID, chat, id)
	if err != nil {
		return Attachment{}, fault(err)
	}
	if a.Chat != chat || a.MessageID != id || a.AccountID != accountID {
		return Attachment{}, Fail("source_changed")
	}
	return a, nil
}

func (s *Service) Start(ctx context.Context, accountID int64, chat string, id int, operation string, options Options, confirmPaid bool) (Job, error) {
	if !confirmPaid {
		return Job{}, Fail("explicit_paid_confirmation_required")
	}
	if operation != "transcription" && operation != "image" {
		return Job{}, Fail("invalid_operation")
	}
	model := s.cfg.AudioModel
	if operation == "image" {
		model = s.cfg.ImageModel
	}
	options, err := normalizeOptions(options, model, operation)
	if err != nil {
		return Job{}, err
	}
	a, err := s.Metadata(ctx, accountID, chat, id)
	if err != nil {
		return Job{}, err
	}
	return s.enqueue(ctx, a, operation, options)
}

func (s *Service) enqueue(ctx context.Context, a Attachment, operation string, options Options) (Job, error) {
	if err := s.checkAttachment(a, operation); err != nil {
		return Job{}, err
	}
	p := jobPayload{Operation: operation, Source: a, Settings: options, Revision: s.revision(options)}
	// Source display metadata is not part of the key: an author rename, caption
	// edit or refreshed file reference must not trigger a second paid recognition.
	key := Digest([]any{a.AccountID, a.Chat, a.MessageID, a.Fingerprint, operation, options, p.Revision})
	if existing, err := s.store.MediaJob(ctx, key); err == nil {
		return decodeJob(existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, Fail("storage_unavailable")
	}
	if !s.cfg.Enabled {
		return Job{}, Fail("cloud_processing_disabled")
	}
	payload, _ := json.Marshal(p)
	j, err := s.store.EnqueueMedia(ctx, db.MediaJob{ID: key, AccountID: a.AccountID, Chat: a.Chat, MessageID: a.MessageID, CreatedAt: s.now().Unix(), Payload: string(payload)})
	if errors.Is(err, db.ErrMediaLimit) {
		return Job{}, Fail("queue_or_result_limit")
	}
	if err != nil {
		return Job{}, Fail("storage_unavailable")
	}
	return decodeJob(j)
}

func (s *Service) Get(ctx context.Context, accountID int64, chat string, id int, jobID string) (Job, error) {
	if err := ValidateReference(chat, id); err != nil {
		return Job{}, err
	}
	if !idPattern.MatchString(jobID) {
		return Job{}, Fail("invalid_job_id")
	}
	j, err := s.store.MediaJob(ctx, jobID)
	if err != nil || accountID <= 0 || j.AccountID != accountID || j.Chat != chat || j.MessageID != id {
		return Job{}, Fail("job_not_found")
	}
	return decodeJob(j)
}

func decodeJob(j db.MediaJob) (Job, error) {
	var p jobPayload
	if json.Unmarshal([]byte(j.Payload), &p) != nil {
		return Job{}, Fail("invalid_job_state")
	}
	out := Job{ID: j.ID, Operation: p.Operation, Status: j.Status, ErrorCode: j.ErrorCode, Attempts: j.Attempts, ProviderAttempts: j.ProviderAttempts, CreatedAt: time.Unix(j.CreatedAt, 0).UTC(), UpdatedAt: time.Unix(j.UpdatedAt, 0).UTC(), Source: p.Source, Settings: p.Settings, SHA256: p.SHA256, Duration: p.Duration}
	// Historical HTTP-200 validation failures also require charge reconciliation.
	// Project the same state without rewriting or requeueing their stored jobs.
	if j.Status == "failed" && j.ProviderAttempts > 0 {
		switch j.ErrorCode {
		case "provider_output_incomplete", "provider_refused_or_missing_result", "provider_invalid_image_result", "provider_output_limit":
			out.Status = "uncertain"
		}
	}
	if j.ResultMeta != "" && j.Status != "completed" {
		var meta failureMeta
		if json.Unmarshal([]byte(j.ResultMeta), &meta) != nil {
			return Job{}, Fail("invalid_result_state")
		}
		out.Provider, out.Budget = meta.Provider, meta.Budget
	}
	if j.Status == "retry_wait" {
		at := time.Unix(j.RetryAt, 0).UTC()
		out.NextAttemptAt = &at
		if j.ErrorCode == "budget_exceeded" {
			out.Status = "budget_wait"
			if j.RetryAt == math.MaxInt64 {
				out.Status = "budget_blocked"
				out.NextAttemptAt = nil
			}
		}
	}
	if j.Status == "completed" {
		var result Result
		if json.Unmarshal([]byte(j.ResultMeta), &result) != nil {
			return Job{}, Fail("invalid_result_state")
		}
		result.Text, result.Description, result.OCRText = j.Transcript, j.ImageDescription, j.ImageText
		result.Operation = p.Operation
		result.Language = nil
		if result.LanguageSource == "provider" && len(result.Languages) == 1 && languagePattern.MatchString(result.Languages[0]) {
			language := result.Languages[0]
			result.Language = &language
		}
		out.Result = &result
	}
	return out, nil
}

func (s *Service) checkAttachment(a Attachment, operation string) error {
	if !a.Supported || a.Fingerprint == "" {
		return Fail("unsupported_attachment")
	}
	if a.Size <= 0 || a.Size > s.cfg.MaxBytes {
		return Fail("attachment_size_limit")
	}
	if operation == "transcription" && a.Kind != "voice" && a.Kind != "video_note" {
		return Fail("unsupported_audio_attachment")
	}
	if operation == "image" && a.Kind != "photo" && a.Kind != "image" {
		return Fail("unsupported_image_attachment")
	}
	if math.IsNaN(a.Duration) || math.IsInf(a.Duration, 0) || a.Duration < 0 || a.Duration > float64(s.cfg.MaxSeconds) {
		return Fail("duration_limit")
	}
	return nil
}

// One bounded worker pool lives in serve and shares the existing Telegram client.
func (s *Service) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return Fail("media_worker_already_running")
	}
	defer s.running.Store(false)
	group, ctx := errgroup.WithContext(ctx)
	for range s.cfg.Concurrency {
		group.Go(func() error { return s.runWorker(ctx) })
	}
	group.Go(func() error {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if err := s.cleanup(ctx, false); err != nil && ctx.Err() == nil {
					return err
				}
			}
		}
	})
	return group.Wait()
}

func (s *Service) runWorker(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if s.cfg.Enabled && len(s.source.LiveAccounts()) > 0 {
			worked, err := s.runOne(ctx)
			if err != nil && ctx.Err() == nil {
				return Fail("media_worker_storage_error")
			}
			if worked && ctx.Err() == nil {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) runOne(ctx context.Context) (bool, error) {
	live := s.source.LiveAccounts()
	j, err := s.store.ClaimMedia(ctx, live, s.now())
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var p jobPayload
	if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
		return true, s.finishError(j, Fail("invalid_job_state"))
	}
	prepared, sha, err := s.prepare(ctx, p.Source, p.Operation)
	if err != nil {
		if ctx.Err() != nil {
			return true, s.finishError(j, Retry("interrupted_before_submission", time.Second))
		}
		return true, s.finishError(j, err)
	}
	p.SHA256, p.Duration = sha, prepared.Duration
	data, _ := json.Marshal(p)
	j.Payload = string(data)
	// 0.006 USD/minute reserved for audio (published price is 0.0045),
	// 0.02 USD for one bounded image completion. Settle known actual costs.
	reserve := int64(20_000)
	if p.Operation == "transcription" {
		reserve = int64(math.Ceil(prepared.Duration)) * 100
	}
	if err := ctx.Err(); err != nil {
		return true, s.finishError(j, Retry("interrupted_before_submission", time.Second))
	}
	if p.Revision != s.revision(p.Settings) {
		return true, s.finishError(j, Fail("configuration_changed"))
	}
	if err := s.waitForProvider(ctx); err != nil {
		return true, s.finishError(j, err)
	}
	if p.Source.AccountID != j.AccountID {
		return true, s.finishError(j, Fail("telegram_account_changed"))
	}
	if !slices.Contains(s.source.LiveAccounts(), j.AccountID) {
		return true, s.finishError(j, Retry("telegram_account_unavailable", time.Minute))
	}
	err = s.store.ReserveMedia(ctx, j.ID, j.Payload, reserve, s.cfg.DailyBudgetMicros, s.cfg.TotalBudgetMicros, s.now())
	var budget *db.MediaBudget
	if errors.As(err, &budget) {
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		slog.Info("media budget reservation refused", "job_id", j.ID, "error_code", "budget_exceeded", "scope", budget.Scope, "required_microusd", budget.Required, "daily_used_microusd", budget.DailyUsed, "total_used_microusd", budget.TotalUsed)
		return true, s.store.DeferMediaBudget(saveCtx, j.ID, budget, s.now())
	}
	if err != nil {
		return true, err
	}
	result, err := s.provider.Recognize(ctx, p.Operation, p.Settings, prepared)
	if err != nil {
		return true, s.finishError(j, err)
	}
	j.Status, j.ErrorCode, j.UpdatedAt = "completed", "", s.now().Unix()
	j.Transcript, j.ImageDescription, j.ImageText = result.Text, result.Description, result.OCRText
	result.Text, result.Description, result.OCRText = "", "", ""
	meta, _ := json.Marshal(result)
	j.ResultMeta = string(meta)
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cost *int64
	if result.CostUSD != nil {
		micros := int64(math.Ceil(*result.CostUSD * 1_000_000))
		cost = &micros
	}
	return true, s.store.FinishMediaWithCost(saveCtx, j, cost)
}

func (s *Service) finishError(j db.MediaJob, err error) error {
	f := fault(err)
	j.Status, j.ErrorCode, j.UpdatedAt = "failed", f.Code, s.now().Unix()
	if f.Uncertain {
		j.Status = "uncertain"
	} else if f.RetryAfter > 0 && j.Attempts < maxAttempts {
		j.Status = "retry_wait"
		j.RetryAt = s.now().Add(max(f.RetryAfter, time.Duration(j.Attempts*j.Attempts)*time.Second)).Unix()
	}
	if f.Code == "openrouter_rate_limited" {
		j.RetryAt = s.now().Add(max(f.RetryAfter, time.Duration(j.Attempts*j.Attempts)*time.Second)).Unix()
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cost *int64
	if f.Provider != nil {
		meta, _ := json.Marshal(failureMeta{Provider: f.Provider})
		j.ResultMeta = string(meta)
		if f.Provider.CostUSD != nil {
			micros := int64(math.Ceil(*f.Provider.CostUSD * 1_000_000))
			cost = &micros
		}
		slog.Info("media provider result requires review", "job_id", j.ID, "error_code", f.Code, "http_status", f.Provider.HTTPStatus, "finish_reason", f.Provider.FinishReason, "failure_reason", f.Provider.FailureReason)
	}
	return s.store.FinishMediaWithCost(saveCtx, j, cost)
}

func (s *Service) revision(options Options) string {
	revision := pipelineVersion + ":" + s.cfg.CacheRevision
	if options.DescriptionLanguage != "" {
		revision += ":image-description-language-v1"
	}
	return revision
}

func (s *Service) waitForProvider(ctx context.Context) error {
	for {
		until, err := s.store.MediaCooldown(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return Retry("interrupted_before_submission", time.Second)
			}
			return Fail("storage_unavailable")
		}
		delay := time.Unix(until, 0).Sub(s.now())
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Retry("interrupted_before_submission", time.Second)
		case <-timer.C:
		}
	}
}

func (s *Service) prepare(ctx context.Context, a Attachment, operation string) (Prepared, string, error) {
	if err := s.checkAttachment(a, operation); err != nil {
		return Prepared{}, "", err
	}
	select {
	case s.files <- struct{}{}:
		defer func() { <-s.files }()
	case <-ctx.Done():
		return Prepared{}, "", Retry("interrupted_before_submission", time.Second)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	f, err := s.downloadLocked(ctx, a)
	if err != nil {
		return Prepared{}, "", err
	}
	dir, err := os.MkdirTemp(s.cfg.Directory, "tmp-")
	if err != nil {
		return Prepared{}, "", Fail("temporary_storage_unavailable")
	}
	defer os.RemoveAll(dir)
	prepared, err := s.processor.Prepare(ctx, filepath.Join(s.cfg.Directory, f.ID+".bin"), dir, a, s.cfg.MaxSeconds)
	return prepared, f.SHA256, err
}
