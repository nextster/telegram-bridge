package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

var ErrMediaLimit = errors.New("media_queue_or_storage_limit")
var ErrMediaBudget = errors.New("media_budget_exceeded")

// Amounts are USD millionths. Original reservations remain in media_charges.
type MediaBudget struct {
	Scope      string `json:"scope"`
	Required   int64  `json:"required_microusd"`
	DailyUsed  int64  `json:"daily_used_microusd"`
	TotalUsed  int64  `json:"total_used_microusd"`
	DailyLimit int64  `json:"daily_limit_microusd"`
	TotalLimit int64  `json:"total_limit_microusd"`
}

func (b *MediaBudget) Error() string { return ErrMediaBudget.Error() }
func (b *MediaBudget) Unwrap() error { return ErrMediaBudget }

func mediaBudget(ctx context.Context, tx *sql.Tx, amount, daily, total int64, now time.Time) (*MediaBudget, error) {
	b := &MediaBudget{Required: amount, DailyLimit: daily, TotalLimit: total}
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN c.day=? THEN COALESCE(s.amount,c.amount) ELSE 0 END),0),
		COALESCE(SUM(COALESCE(s.amount,c.amount)),0) FROM media_charges c LEFT JOIN media_charge_settlements s ON s.charge_id=c.id`, now.UTC().Format(time.DateOnly)).Scan(&b.DailyUsed, &b.TotalUsed)
	if amount > daily || amount > total {
		b.Scope = "request"
	} else if b.TotalUsed > total-amount {
		b.Scope = "total"
	} else if b.DailyUsed > daily-amount {
		b.Scope = "daily"
	}
	return b, err
}

type MediaJob struct {
	ID               string
	AccountID        int64
	Chat             string
	MessageID        int
	Status           string
	Attempts         int
	ProviderAttempts int
	RetryAt          int64
	CreatedAt        int64
	UpdatedAt        int64
	ErrorCode        string
	Payload          string
	Transcript       string
	ImageDescription string
	ImageText        string
	ResultMeta       string
}

const mediaColumns = `id, account_id, chat, message_id, status, attempts, provider_attempts, retry_at, created_at, updated_at, error_code, payload, transcript, image_description, image_text, result_meta`

func scanMediaJob(row interface{ Scan(...any) error }) (j MediaJob, err error) {
	err = row.Scan(&j.ID, &j.AccountID, &j.Chat, &j.MessageID, &j.Status, &j.Attempts, &j.ProviderAttempts, &j.RetryAt, &j.CreatedAt, &j.UpdatedAt, &j.ErrorCode, &j.Payload, &j.Transcript, &j.ImageDescription, &j.ImageText, &j.ResultMeta)
	return
}

func (s *Store) MediaJob(ctx context.Context, id string) (MediaJob, error) {
	return scanMediaJob(s.db.QueryRowContext(ctx, `SELECT `+mediaColumns+` FROM media_jobs WHERE id=?`, id))
}

func (s *Store) EnqueueMedia(ctx context.Context, j MediaJob) (MediaJob, error) {
	if existing, err := s.MediaJob(ctx, j.ID); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MediaJob{}, err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO media_jobs (id, account_id, chat, message_id, status, created_at, updated_at, payload)
		SELECT ?,?,?,?,'queued',?,?,? WHERE (SELECT COUNT(*) FROM media_jobs) < 1000
		AND (SELECT COUNT(*) FROM media_jobs WHERE status IN ('queued','preparing','submitting','retry_wait')) < 100
		ON CONFLICT(id) DO NOTHING`, j.ID, j.AccountID, j.Chat, j.MessageID, j.CreatedAt, j.CreatedAt, j.Payload)
	if err != nil {
		return MediaJob{}, err
	}
	result, err := s.MediaJob(ctx, j.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaJob{}, ErrMediaLimit
	}
	return result, err
}

// ClaimMedia claims the oldest runnable job of any account that currently has
// a live Telegram session. A job is always processed with its own account.
func (s *Store) ClaimMedia(ctx context.Context, accountIDs []int64, now time.Time) (MediaJob, error) {
	if len(accountIDs) == 0 {
		return MediaJob{}, sql.ErrNoRows
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(accountIDs)), ",")
	args := []any{now.Unix()}
	for _, id := range accountIDs {
		args = append(args, id)
	}
	args = append(args, now.Unix(), now.Unix())
	return scanMediaJob(s.db.QueryRowContext(ctx, `UPDATE media_jobs SET status='preparing', attempts=attempts+1, updated_at=?
		WHERE id=(SELECT id FROM media_jobs WHERE account_id IN (`+placeholders+`) AND status IN ('queued','retry_wait') AND retry_at<=?
		AND NOT EXISTS(SELECT 1 FROM media_jobs WHERE error_code='openrouter_rate_limited' AND retry_at>?)
		ORDER BY created_at,id LIMIT 1)
		RETURNING `+mediaColumns, args...))
}

// A persisted 429 pauses every worker, including after restart.
func (s *Store) MediaCooldown(ctx context.Context) (int64, error) {
	var until int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(retry_at),0) FROM media_jobs WHERE error_code='openrouter_rate_limited'`).Scan(&until)
	return until, err
}

func (s *Store) RecoverMedia(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media_jobs SET
		status=CASE WHEN status='submitting' THEN 'uncertain' WHEN attempts>=3 THEN 'failed' ELSE 'queued' END,
		error_code=CASE WHEN status='submitting' THEN 'provider_outcome_unknown' WHEN attempts>=3 THEN 'retry_exhausted' ELSE '' END,
		updated_at=? WHERE status IN ('preparing','submitting')`, now.Unix())
	return err
}

// Reserve and mark submitting in one transaction, before any billable request.
// Unknown costs retain their reservation; verified costs replace it for limits.
func (s *Store) ReserveMedia(ctx context.Context, id, payload string, amount, daily, total int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE media_jobs SET status='submitting', provider_attempts=provider_attempts+1, payload=?,updated_at=? WHERE id=? AND status='preparing'`, payload, now.Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("media_job_not_claimed")
	}
	budget, err := mediaBudget(ctx, tx, amount, daily, total, now)
	if err != nil {
		return err
	}
	if budget.Scope != "" {
		return budget
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO media_charges(job_id,day,amount) VALUES(?,?,?)`, id, now.UTC().Format(time.DateOnly), amount); err != nil {
		return err
	}
	return tx.Commit()
}

// Stored as retry_wait for additive-schema and old-binary compatibility. The
// API distinguishes budget_wait/budget_blocked from bounded failure retries.
func (s *Store) DeferMediaBudget(ctx context.Context, id string, b *MediaBudget, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE media_jobs SET status='retry_wait',error_code='budget_exceeded',attempts=MAX(attempts-1,0),updated_at=? WHERE id=? AND status='preparing'`, now.Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("media_job_not_claimed")
	}
	// Recheck under the write lock: another request may have settled since the
	// failed reservation. Do not miss that wakeup and wait until tomorrow.
	b, err = mediaBudget(ctx, tx, b.Required, b.DailyLimit, b.TotalLimit, now)
	if err != nil {
		return err
	}
	retryAt := int64(math.MaxInt64)
	if b.Scope == "daily" {
		retryAt = now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Unix()
	}
	if b.Scope == "" {
		if _, err = tx.ExecContext(ctx, `UPDATE media_jobs SET status='queued',error_code='',retry_at=0,result_meta='' WHERE id=?`, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	meta, err := json.Marshal(struct {
		Budget *MediaBudget `json:"budget"`
	}{b})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE media_jobs SET retry_at=?,result_meta=? WHERE id=?`, retryAt, string(meta), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishMedia(ctx context.Context, j MediaJob) error {
	return s.FinishMediaWithCost(ctx, j, nil)
}

// Persist the result/diagnostics and its verified cost together. A crash cannot
// release a reserve while losing the metadata needed to explain the settlement.
func (s *Store) FinishMediaWithCost(ctx context.Context, j MediaJob, micros *int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE media_jobs SET status=?, retry_at=?, updated_at=?, error_code=?, payload=?,
		transcript=?,image_description=?,image_text=?,result_meta=? WHERE id=? AND status IN ('preparing','submitting')`,
		j.Status, j.RetryAt, j.UpdatedAt, j.ErrorCode, j.Payload, j.Transcript, j.ImageDescription, j.ImageText, j.ResultMeta, j.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("media_job_not_claimed")
	}
	if micros != nil {
		if err = recordMediaCost(ctx, tx, j.ID, *micros); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecordMediaCost(ctx context.Context, id string, micros int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = recordMediaCost(ctx, tx, id, micros); err != nil {
		return err
	}
	return tx.Commit()
}

func recordMediaCost(ctx context.Context, tx *sql.Tx, id string, micros int64) error {
	if micros < 0 {
		return errors.New("invalid_media_cost")
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO media_charge_settlements(charge_id,amount)
		SELECT id,? FROM media_charges WHERE id=(SELECT MAX(id) FROM media_charges WHERE job_id=?)
		ON CONFLICT(charge_id) DO UPDATE SET amount=MAX(amount,excluded.amount)`, micros, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("media_charge_not_found")
	}
	// Retry only jobs already waiting for budget, never historical failed jobs.
	if _, err = tx.ExecContext(ctx, `UPDATE media_jobs SET retry_at=0 WHERE status='retry_wait' AND error_code='budget_exceeded'`); err != nil {
		return err
	}
	return nil
}

// Reconcile only known completed costs, against the last submission's charge.
// Earlier attempts and failures without verified costs keep their reservations.
// This does not requeue failed jobs or alter their identity/results.
func (s *Store) ReconcileMediaCosts(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id,j.result_meta FROM media_jobs j
		WHERE j.status='completed' AND NOT EXISTS(SELECT 1 FROM media_charge_settlements WHERE charge_id=(SELECT MAX(id) FROM media_charges WHERE job_id=j.id))`)
	if err != nil {
		return err
	}
	type cost struct {
		id     string
		micros int64
	}
	costs := []cost{}
	for rows.Next() {
		var id, meta string
		if err = rows.Scan(&id, &meta); err != nil {
			rows.Close()
			return err
		}
		var m struct {
			Cost *float64 `json:"cost_usd"`
		}
		if json.Unmarshal([]byte(meta), &m) == nil && m.Cost != nil && *m.Cost >= 0 && *m.Cost <= 1_000_000 && !math.IsNaN(*m.Cost) && !math.IsInf(*m.Cost, 0) {
			costs = append(costs, cost{id, int64(math.Ceil(*m.Cost * 1_000_000))})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range costs {
		if err = s.RecordMediaCost(ctx, c.id, c.micros); err != nil {
			return err
		}
	}
	return nil
}

type MediaFile struct {
	ID          string
	AccountID   int64
	Chat        string
	MessageID   int
	Fingerprint string
	Size        int64
	SHA256      string
	MIME        string
	ExpiresAt   int64
}

func (s *Store) PutMediaFile(ctx context.Context, f MediaFile) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO media_files(id,account_id,chat,message_id,fingerprint,size,sha256,mime,expires_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET size=excluded.size,sha256=excluded.sha256,mime=excluded.mime,expires_at=excluded.expires_at`, f.ID, f.AccountID, f.Chat, f.MessageID, f.Fingerprint, f.Size, f.SHA256, f.MIME, f.ExpiresAt)
	return err
}
func (s *Store) MediaFile(ctx context.Context, id string) (f MediaFile, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id,account_id,chat,message_id,fingerprint,size,sha256,mime,expires_at FROM media_files WHERE id=?`, id).Scan(&f.ID, &f.AccountID, &f.Chat, &f.MessageID, &f.Fingerprint, &f.Size, &f.SHA256, &f.MIME, &f.ExpiresAt)
	return
}
func (s *Store) PruneMediaFiles(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media_files WHERE expires_at<=?`, now)
	return err
}
