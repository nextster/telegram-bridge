package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrMediaLimit = errors.New("media_queue_or_storage_limit")
var ErrMediaBudget = errors.New("media_budget_exceeded")

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

func (s *Store) ClaimMedia(ctx context.Context, accountID int64, now time.Time) (MediaJob, error) {
	return scanMediaJob(s.db.QueryRowContext(ctx, `UPDATE media_jobs SET status='preparing', attempts=attempts+1, updated_at=?
		WHERE id=(SELECT id FROM media_jobs WHERE account_id=? AND status IN ('queued','retry_wait') AND retry_at<=? ORDER BY created_at,id LIMIT 1)
		RETURNING `+mediaColumns, now.Unix(), accountID, now.Unix()))
}

func (s *Store) RecoverMedia(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media_jobs SET
		status=CASE WHEN status='submitting' THEN 'uncertain' WHEN attempts>=3 THEN 'failed' ELSE 'queued' END,
		error_code=CASE WHEN status='submitting' THEN 'provider_outcome_unknown' WHEN attempts>=3 THEN 'retry_exhausted' ELSE '' END,
		updated_at=? WHERE status IN ('preparing','submitting')`, now.Unix())
	return err
}

// Reserve and mark submitting in one transaction, before any billable request.
// Reservations are retained even for rejected/uncertain calls, conservatively.
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
	day := now.UTC().Format(time.DateOnly)
	res, err = tx.ExecContext(ctx, `INSERT INTO media_charges(job_id,day,amount) SELECT ?,?,?
		WHERE COALESCE((SELECT SUM(amount) FROM media_charges WHERE day=?),0)+?<=?
		AND COALESCE((SELECT SUM(amount) FROM media_charges),0)+?<=?`, id, day, amount, day, amount, daily, amount, total)
	if err != nil {
		return err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMediaBudget
	}
	return tx.Commit()
}

func (s *Store) FinishMedia(ctx context.Context, j MediaJob) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media_jobs SET status=?, retry_at=?, updated_at=?, error_code=?, payload=?,
		transcript=?,image_description=?,image_text=?,result_meta=? WHERE id=? AND status IN ('preparing','submitting')`,
		j.Status, j.RetryAt, j.UpdatedAt, j.ErrorCode, j.Payload, j.Transcript, j.ImageDescription, j.ImageText, j.ResultMeta, j.ID)
	return err
}

func (s *Store) RecordMediaCost(ctx context.Context, id string, micros int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE media_charges SET amount=MAX(amount,?) WHERE id=(SELECT MAX(id) FROM media_charges WHERE job_id=?)`, micros, id)
	return err
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
