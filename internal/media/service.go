package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
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
	return s, nil
}
func (s *Service) Close() error { return s.root.Close() }

func (s *Service) Metadata(ctx context.Context, chat string, id int) (Attachment, error) {
	if err := ValidateReference(chat, id); err != nil {
		return Attachment{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a, err := s.source.Attachment(ctx, chat, id)
	if err != nil {
		return Attachment{}, fault(err)
	}
	if a.Chat != chat || a.MessageID != id || a.AccountID != s.source.AccountID() {
		return Attachment{}, Fail("source_changed")
	}
	return a, nil
}

func (s *Service) Start(ctx context.Context, chat string, id int, operation string, options Options, confirmPaid bool) (Job, error) {
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
	a, err := s.Metadata(ctx, chat, id)
	if err != nil {
		return Job{}, err
	}
	return s.enqueue(ctx, a, operation, options)
}

func (s *Service) enqueue(ctx context.Context, a Attachment, operation string, options Options) (Job, error) {
	if err := s.checkAttachment(a, operation); err != nil {
		return Job{}, err
	}
	p := jobPayload{Operation: operation, Source: a, Settings: options, Revision: pipelineVersion + ":" + s.cfg.CacheRevision}
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

func (s *Service) Get(ctx context.Context, chat string, id int, jobID string) (Job, error) {
	if err := ValidateReference(chat, id); err != nil {
		return Job{}, err
	}
	if !idPattern.MatchString(jobID) {
		return Job{}, Fail("invalid_job_id")
	}
	j, err := s.store.MediaJob(ctx, jobID)
	if err != nil || j.AccountID != s.source.AccountID() || j.Chat != chat || j.MessageID != id {
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
	if j.Status == "retry_wait" {
		at := time.Unix(j.RetryAt, 0).UTC()
		out.NextAttemptAt = &at
	}
	if j.Status == "completed" {
		var result Result
		if json.Unmarshal([]byte(j.ResultMeta), &result) != nil {
			return Job{}, Fail("invalid_result_state")
		}
		result.Text, result.Description, result.OCRText = j.Transcript, j.ImageDescription, j.ImageText
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
		if s.cfg.Enabled && s.source.AccountID() != 0 {
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
	j, err := s.store.ClaimMedia(ctx, s.source.AccountID(), s.now())
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
		return true, s.finishError(j, err)
	}
	p.SHA256, p.Duration = sha, prepared.Duration
	data, _ := json.Marshal(p)
	j.Payload = string(data)
	// 0.006 USD/minute reserved for audio (published price is 0.0045),
	// 0.02 USD for one bounded image completion. Keep failed reservations too.
	reserve := int64(20_000)
	if p.Operation == "transcription" {
		reserve = int64(math.Ceil(prepared.Duration)) * 100
	}
	if err := ctx.Err(); err != nil {
		return true, s.finishError(j, Retry("interrupted_before_submission", time.Second))
	}
	if p.Revision != pipelineVersion+":"+s.cfg.CacheRevision {
		return true, s.finishError(j, Fail("configuration_changed"))
	}
	if err := s.waitForProvider(ctx); err != nil {
		return true, s.finishError(j, err)
	}
	if s.source.AccountID() != p.Source.AccountID {
		return true, s.finishError(j, Fail("telegram_account_changed"))
	}
	err = s.store.ReserveMedia(ctx, j.ID, j.Payload, reserve, s.cfg.DailyBudgetMicros, s.cfg.TotalBudgetMicros, s.now())
	if errors.Is(err, db.ErrMediaBudget) {
		return true, s.finishError(j, Fail("budget_exceeded"))
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
	if result.CostUSD != nil {
		if err := s.store.RecordMediaCost(saveCtx, j.ID, int64(math.Ceil(*result.CostUSD*1_000_000))); err != nil {
			return true, err
		}
	}
	return true, s.store.FinishMedia(saveCtx, j)
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
	return s.store.FinishMedia(saveCtx, j)
}

func (s *Service) waitForProvider(ctx context.Context) error {
	for {
		until, err := s.store.MediaCooldown(ctx)
		if err != nil {
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
