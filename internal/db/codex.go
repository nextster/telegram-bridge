package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CodexProject struct {
	Slug               string    `json:"slug"`
	Title              string    `json:"title"`
	TelegramChannelID  int64     `json:"telegram_channel_id"`
	TelegramAccessHash int64     `json:"-"`
	TelegramChatID     int64     `json:"telegram_chat_id"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type CodexThread struct {
	ID              int64     `json:"id"`
	ProjectSlug     string    `json:"project_slug"`
	TelegramChatID  int64     `json:"telegram_chat_id"`
	TelegramTopicID int       `json:"telegram_topic_id"`
	Title           string    `json:"title"`
	CodexThreadID   string    `json:"codex_thread_id"`
	CWD             string    `json:"cwd,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CodexThreadSnapshot struct {
	CodexThreadID string   `json:"thread_id"`
	Title         string   `json:"title"`
	CWD           string   `json:"cwd"`
	Status        string   `json:"status"`
	ActiveFlags   []string `json:"active_flags,omitempty"`
	MessageRole   string   `json:"message_role,omitempty"`
	Message       string   `json:"message,omitempty"`
	UpdatedAt     int64    `json:"updated_at"`
	Unread        *bool    `json:"unread,omitempty"`
}

type CodexThreadMirror struct {
	Thread            CodexThread
	TelegramMessageID int
	ContentHash       string
}

type CodexJob struct {
	ID             string      `json:"id"`
	ThreadID       int64       `json:"thread_id"`
	Prompt         string      `json:"prompt"`
	Status         string      `json:"status"`
	WorkerID       string      `json:"worker_id"`
	LeaseToken     string      `json:"lease_token,omitempty"`
	LeaseExpiresAt time.Time   `json:"lease_expires_at"`
	Result         string      `json:"result,omitempty"`
	Error          string      `json:"error,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	StartedAt      time.Time   `json:"started_at"`
	FinishedAt     time.Time   `json:"finished_at"`
	Thread         CodexThread `json:"thread"`
}

func (s *Store) UpsertCodexProject(ctx context.Context, p CodexProject) error {
	p.Slug = strings.TrimSpace(p.Slug)
	p.Title = strings.TrimSpace(p.Title)
	if p.Slug == "" || p.TelegramChannelID <= 0 || p.TelegramChatID == 0 {
		return errors.New("invalid Codex project")
	}
	if p.Title == "" {
		p.Title = p.Slug
	}
	now := nowText()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_projects(slug, title, telegram_channel_id, telegram_access_hash, telegram_chat_id, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			title = excluded.title,
			telegram_channel_id = excluded.telegram_channel_id,
			telegram_access_hash = excluded.telegram_access_hash,
			telegram_chat_id = excluded.telegram_chat_id,
			updated_at = excluded.updated_at
	`, p.Slug, p.Title, p.TelegramChannelID, p.TelegramAccessHash, p.TelegramChatID, now, now)
	if err != nil {
		return fmt.Errorf("upsert Codex project: %w", err)
	}
	return nil
}

func (s *Store) GetCodexProject(ctx context.Context, slug string) (CodexProject, bool, error) {
	var p CodexProject
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT slug, title, telegram_channel_id, telegram_access_hash, telegram_chat_id, created_at, updated_at
		FROM codex_projects WHERE slug = ? COLLATE NOCASE
	`, strings.TrimSpace(slug)).Scan(&p.Slug, &p.Title, &p.TelegramChannelID, &p.TelegramAccessHash, &p.TelegramChatID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexProject{}, false, nil
	}
	if err != nil {
		return CodexProject{}, false, fmt.Errorf("get Codex project: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = parseDBTime(created), parseDBTime(updated)
	return p, true, nil
}

func (s *Store) GetCodexProjectByChatID(ctx context.Context, chatID int64) (CodexProject, bool, error) {
	var p CodexProject
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT slug, title, telegram_channel_id, telegram_access_hash, telegram_chat_id, created_at, updated_at
		FROM codex_projects WHERE telegram_chat_id = ?
	`, chatID).Scan(&p.Slug, &p.Title, &p.TelegramChannelID, &p.TelegramAccessHash, &p.TelegramChatID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexProject{}, false, nil
	}
	if err != nil {
		return CodexProject{}, false, fmt.Errorf("get Codex project by Telegram chat: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = parseDBTime(created), parseDBTime(updated)
	return p, true, nil
}

func (s *Store) ListCodexProjects(ctx context.Context) ([]CodexProject, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT slug, title, telegram_channel_id, telegram_access_hash, telegram_chat_id, created_at, updated_at
		FROM codex_projects ORDER BY title COLLATE NOCASE
	`)
	if err != nil {
		return nil, fmt.Errorf("list Codex projects: %w", err)
	}
	defer rows.Close()
	var projects []CodexProject
	for rows.Next() {
		var p CodexProject
		var created, updated string
		if err := rows.Scan(&p.Slug, &p.Title, &p.TelegramChannelID, &p.TelegramAccessHash, &p.TelegramChatID, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan Codex project: %w", err)
		}
		p.CreatedAt, p.UpdatedAt = parseDBTime(created), parseDBTime(updated)
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) CreateCodexThread(ctx context.Context, t CodexThread) (CodexThread, error) {
	now := nowText()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_threads(project_slug, telegram_chat_id, telegram_topic_id, title, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)
	`, t.ProjectSlug, t.TelegramChatID, t.TelegramTopicID, strings.TrimSpace(t.Title), now, now)
	if err != nil {
		return CodexThread{}, fmt.Errorf("create Codex thread: %w", err)
	}
	t.ID, err = res.LastInsertId()
	if err != nil {
		return CodexThread{}, fmt.Errorf("Codex thread id: %w", err)
	}
	t.CreatedAt, t.UpdatedAt = parseDBTime(now), parseDBTime(now)
	return t, nil
}

func (s *Store) GetCodexThreadByTopic(ctx context.Context, chatID int64, topicID int) (CodexThread, bool, error) {
	return s.scanCodexThread(s.db.QueryRowContext(ctx, `
		SELECT id, project_slug, telegram_chat_id, telegram_topic_id, title, codex_thread_id, created_at, updated_at
		FROM codex_threads WHERE telegram_chat_id = ? AND telegram_topic_id = ?
	`, chatID, topicID))
}

func (s *Store) ListCodexThreadsByChatID(ctx context.Context, chatID int64) ([]CodexThread, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_slug, telegram_chat_id, telegram_topic_id, title, codex_thread_id, created_at, updated_at
		FROM codex_threads WHERE telegram_chat_id = ? ORDER BY telegram_topic_id
	`, chatID)
	if err != nil {
		return nil, fmt.Errorf("list Codex threads by Telegram chat: %w", err)
	}
	defer rows.Close()
	var threads []CodexThread
	for rows.Next() {
		var thread CodexThread
		var created, updated string
		if err := rows.Scan(&thread.ID, &thread.ProjectSlug, &thread.TelegramChatID, &thread.TelegramTopicID,
			&thread.Title, &thread.CodexThreadID, &created, &updated); err != nil {
			return nil, err
		}
		thread.CreatedAt, thread.UpdatedAt = parseDBTime(created), parseDBTime(updated)
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

func (s *Store) GetCodexThreadMirrorByCodexID(ctx context.Context, codexThreadID string) (CodexThreadMirror, bool, error) {
	var mirror CodexThreadMirror
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.project_slug, t.telegram_chat_id, t.telegram_topic_id, t.title,
			t.codex_thread_id, t.created_at, t.updated_at,
			COALESCE(m.cwd, ''), COALESCE(m.telegram_message_id, 0), COALESCE(m.content_hash, '')
		FROM codex_thread_aliases a
		JOIN codex_threads t ON t.id = a.thread_id
		LEFT JOIN codex_thread_mirrors m ON m.thread_id = t.id
		WHERE a.codex_thread_id = ?
	`, strings.TrimSpace(codexThreadID)).Scan(&mirror.Thread.ID, &mirror.Thread.ProjectSlug,
		&mirror.Thread.TelegramChatID, &mirror.Thread.TelegramTopicID, &mirror.Thread.Title,
		&mirror.Thread.CodexThreadID, &created, &updated, &mirror.Thread.CWD,
		&mirror.TelegramMessageID, &mirror.ContentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexThreadMirror{}, false, nil
	}
	if err != nil {
		return CodexThreadMirror{}, false, fmt.Errorf("get Codex thread mirror: %w", err)
	}
	mirror.Thread.CreatedAt, mirror.Thread.UpdatedAt = parseDBTime(created), parseDBTime(updated)
	return mirror, true, nil
}

func (s *Store) IsCodexTopicDeleted(ctx context.Context, threadID int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_deleted_topics WHERE thread_id = ?)`, threadID).Scan(&exists)
	return exists != 0, err
}

func (s *Store) SaveCodexThreadMirror(ctx context.Context, threadID int64, cwd string, telegramMessageID int, contentHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_thread_mirrors(thread_id, cwd, telegram_message_id, content_hash, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(thread_id) DO UPDATE SET
			cwd = excluded.cwd,
			telegram_message_id = CASE WHEN excluded.telegram_message_id > 0 THEN excluded.telegram_message_id ELSE codex_thread_mirrors.telegram_message_id END,
			content_hash = excluded.content_hash,
			updated_at = excluded.updated_at
	`, threadID, strings.TrimSpace(cwd), telegramMessageID, strings.TrimSpace(contentHash), nowText())
	if err != nil {
		return fmt.Errorf("save Codex thread mirror: %w", err)
	}
	return nil
}

func (s *Store) ObserveCodexTopicReadState(ctx context.Context, telegramChatID int64, telegramTopicID int, unread bool, readMaxID int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var threadID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM codex_threads WHERE telegram_chat_id = ? AND telegram_topic_id = ?`, telegramChatID, telegramTopicID).Scan(&threadID); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var previousUnread bool
	var previousReadMax int
	stateExists := true
	if err := tx.QueryRowContext(ctx, `SELECT is_unread, read_max_id FROM codex_topic_read_states WHERE thread_id = ?`, threadID).Scan(&previousUnread, &previousReadMax); errors.Is(err, sql.ErrNoRows) {
		stateExists = false
	} else if err != nil {
		return false, err
	}
	now := nowText()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO codex_topic_read_states(thread_id, is_unread, read_max_id, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(thread_id) DO UPDATE SET is_unread = excluded.is_unread, read_max_id = excluded.read_max_id, updated_at = excluded.updated_at
	`, threadID, unread, readMaxID, now); err != nil {
		return false, err
	}
	shouldDeliver := !unread && (!stateExists || previousUnread || readMaxID > previousReadMax)
	if shouldDeliver {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO codex_read_receipts(thread_id, requested_at, delivered_at) VALUES(?, ?, '')
			ON CONFLICT(thread_id) DO UPDATE SET requested_at = excluded.requested_at, delivered_at = ''
		`, threadID, now); err != nil {
			return false, err
		}
	}
	return shouldDeliver, tx.Commit()
}

func (s *Store) PendingCodexReadReceipts(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(t.codex_thread_id, ''), a.codex_thread_id)
		FROM codex_read_receipts r
		JOIN codex_threads t ON t.id = r.thread_id
		LEFT JOIN codex_thread_aliases a ON a.thread_id = t.id
		WHERE r.delivered_at = ''
		GROUP BY t.id
		ORDER BY r.requested_at
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list Codex read receipts: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (s *Store) AckCodexReadReceipts(ctx context.Context, threadIDs []string) error {
	for _, id := range threadIDs {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE codex_read_receipts SET delivered_at = ? WHERE thread_id IN (
				SELECT thread_id FROM codex_thread_aliases WHERE codex_thread_id = ?
				UNION SELECT id FROM codex_threads WHERE codex_thread_id = ?
			)
		`, nowText(), strings.TrimSpace(id), strings.TrimSpace(id)); err != nil {
			return fmt.Errorf("ack Codex read receipt: %w", err)
		}
	}
	return nil
}

func (s *Store) ReserveCodexOutboundMessage(ctx context.Context, chatID int64, topicID int, text string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO codex_outbound_messages(telegram_chat_id, telegram_topic_id, telegram_message_id, text, created_at)
		VALUES(?, ?, 0, ?, ?)
	`, chatID, topicID, text, nowText())
	return err
}

func (s *Store) CompleteCodexOutboundMessage(ctx context.Context, chatID int64, topicID, messageID int, text string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM codex_outbound_messages WHERE telegram_chat_id = ? AND telegram_topic_id = ? AND telegram_message_id = 0`, chatID, topicID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO codex_outbound_messages(telegram_chat_id, telegram_topic_id, telegram_message_id, text, created_at)
		VALUES(?, ?, ?, ?, ?)
	`, chatID, topicID, messageID, text, nowText())
	return err
}

func (s *Store) CancelCodexOutboundMessage(ctx context.Context, chatID int64, topicID int) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM codex_outbound_messages WHERE telegram_chat_id = ? AND telegram_topic_id = ? AND telegram_message_id = 0`, chatID, topicID)
}

func (s *Store) ConsumeCodexOutboundMessage(ctx context.Context, chatID int64, topicID, messageID int, text string) (bool, error) {
	cutoff := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM codex_outbound_messages
		WHERE telegram_chat_id = ? AND telegram_topic_id = ? AND created_at >= ?
		AND (telegram_message_id = ? OR (telegram_message_id = 0 AND text = ?))
	`, chatID, topicID, cutoff, messageID, text)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) SetCodexThreadTitle(ctx context.Context, threadID int64, title string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE codex_threads SET title = ?, updated_at = ? WHERE id = ?",
		strings.TrimSpace(title), nowText(), threadID)
	if err != nil {
		return fmt.Errorf("set Codex thread title: %w", err)
	}
	return nil
}

func (s *Store) SetCodexThreadID(ctx context.Context, threadID int64, codexThreadID string) error {
	codexThreadID = strings.TrimSpace(codexThreadID)
	_, err := s.db.ExecContext(ctx, `UPDATE codex_threads SET codex_thread_id = ?, updated_at = ? WHERE id = ?`, codexThreadID, nowText(), threadID)
	if err != nil {
		return fmt.Errorf("set Codex thread id: %w", err)
	}
	if codexThreadID != "" {
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO codex_thread_aliases(thread_id, codex_thread_id, observed_at) VALUES(?, ?, ?)
		`, threadID, codexThreadID, nowText()); err != nil {
			return fmt.Errorf("remember Codex thread alias: %w", err)
		}
	}
	return nil
}

func (s *Store) EnqueueCodexJob(ctx context.Context, threadID int64, prompt string) (CodexJob, error) {
	prompt = strings.TrimSpace(prompt)
	if threadID <= 0 || prompt == "" {
		return CodexJob{}, errors.New("thread and prompt are required")
	}
	id, err := randomCodexToken(18)
	if err != nil {
		return CodexJob{}, err
	}
	now := nowText()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO codex_jobs(id, thread_id, prompt, status, created_at, updated_at)
		VALUES(?, ?, ?, 'queued', ?, ?)
	`, id, threadID, prompt, now, now)
	if err != nil {
		return CodexJob{}, fmt.Errorf("enqueue Codex job: %w", err)
	}
	return CodexJob{ID: id, ThreadID: threadID, Prompt: prompt, Status: "queued", CreatedAt: parseDBTime(now), UpdatedAt: parseDBTime(now)}, nil
}

func (s *Store) ClaimCodexJob(ctx context.Context, workerID string, lease time.Duration) (CodexJob, bool, error) {
	if lease <= 0 {
		lease = 10 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodexJob{}, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM codex_jobs
		WHERE status = 'queued' OR (status IN ('claimed', 'running') AND lease_expires_at != '' AND lease_expires_at < ?)
		ORDER BY created_at LIMIT 1
	`, formatTime(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexJob{}, false, nil
	}
	if err != nil {
		return CodexJob{}, false, fmt.Errorf("find Codex job: %w", err)
	}
	leaseToken, err := randomCodexToken(24)
	if err != nil {
		return CodexJob{}, false, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE codex_jobs SET status = 'claimed', worker_id = ?, lease_token = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ?
	`, strings.TrimSpace(workerID), leaseToken, formatTime(now.Add(lease)), formatTime(now), id)
	if err != nil {
		return CodexJob{}, false, fmt.Errorf("claim Codex job: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return CodexJob{}, false, nil
	}
	job, err := scanCodexJob(tx.QueryRowContext(ctx, codexJobSelect+` WHERE j.id = ?`, id))
	if err != nil {
		return CodexJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return CodexJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) StartCodexJob(ctx context.Context, id, leaseToken, codexThreadID string) error {
	now := nowText()
	res, err := s.db.ExecContext(ctx, `
		UPDATE codex_jobs SET status = 'running', started_at = CASE WHEN started_at = '' THEN ? ELSE started_at END, updated_at = ?
		WHERE id = ? AND lease_token = ? AND status = 'claimed'
	`, now, now, id, leaseToken)
	if err != nil {
		return fmt.Errorf("start Codex job: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return errors.New("Codex job lease is stale")
	}
	if strings.TrimSpace(codexThreadID) != "" {
		var threadID int64
		if err = s.db.QueryRowContext(ctx, `SELECT thread_id FROM codex_jobs WHERE id = ?`, id).Scan(&threadID); err == nil {
			err = s.SetCodexThreadID(ctx, threadID, codexThreadID)
		}
	}
	return err
}

func (s *Store) PendingArchivedCodexThreads(ctx context.Context, archivedIDs []string) ([]CodexThread, error) {
	archived := make(map[string]struct{}, len(archivedIDs))
	for _, id := range archivedIDs {
		if id = strings.TrimSpace(id); id != "" {
			archived[id] = struct{}{}
		}
	}
	if len(archived) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT t.id, t.project_slug, t.telegram_chat_id, t.telegram_topic_id, t.title,
			t.codex_thread_id, t.created_at, t.updated_at, a.codex_thread_id
		FROM codex_threads t
		JOIN codex_thread_aliases a ON a.thread_id = t.id
		LEFT JOIN codex_deleted_topics d ON d.thread_id = t.id
		WHERE d.thread_id IS NULL
`)
	if err != nil {
		return nil, fmt.Errorf("list pending archived Codex threads: %w", err)
	}
	defer rows.Close()
	byID := make(map[int64]CodexThread)
	for rows.Next() {
		var thread CodexThread
		var created, updated, alias string
		if err := rows.Scan(&thread.ID, &thread.ProjectSlug, &thread.TelegramChatID, &thread.TelegramTopicID,
			&thread.Title, &thread.CodexThreadID, &created, &updated, &alias); err != nil {
			return nil, fmt.Errorf("scan pending archived Codex thread: %w", err)
		}
		if _, ok := archived[alias]; !ok {
			continue
		}
		thread.CreatedAt, thread.UpdatedAt = parseDBTime(created), parseDBTime(updated)
		byID[thread.ID] = thread
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	threads := make([]CodexThread, 0, len(byID))
	for _, thread := range byID {
		threads = append(threads, thread)
	}
	return threads, nil
}

func (s *Store) MarkCodexTopicDeleted(ctx context.Context, threadID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO codex_deleted_topics(thread_id, deleted_at) VALUES(?, ?)
	`, threadID, nowText())
	if err != nil {
		return fmt.Errorf("mark Codex topic deleted: %w", err)
	}
	return nil
}

func (s *Store) FinishCodexJob(ctx context.Context, id, leaseToken, resultText, errorText string) (CodexJob, error) {
	status := "succeeded"
	if strings.TrimSpace(errorText) != "" {
		status = "failed"
	}
	now := nowText()
	res, err := s.db.ExecContext(ctx, `
		UPDATE codex_jobs SET status = ?, result = ?, error = ?, finished_at = ?, updated_at = ?, lease_expires_at = ''
		WHERE id = ? AND lease_token = ? AND status IN ('claimed', 'running')
	`, status, strings.TrimSpace(resultText), strings.TrimSpace(errorText), now, now, id, leaseToken)
	if err != nil {
		return CodexJob{}, fmt.Errorf("finish Codex job: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return CodexJob{}, errors.New("Codex job lease is stale")
	}
	return scanCodexJob(s.db.QueryRowContext(ctx, codexJobSelect+` WHERE j.id = ?`, id))
}

func (s *Store) RetryCodexJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE codex_jobs SET
			status = 'queued', worker_id = '', lease_token = '', lease_expires_at = '',
			result = '', error = '', updated_at = ?, started_at = '', finished_at = ''
		WHERE id = ? AND status = 'failed'
	`, nowText(), strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("retry Codex job: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return errors.New("Codex job is missing or is not failed")
	}
	return nil
}

const codexJobSelect = `
	SELECT j.id, j.thread_id, j.prompt, j.status, j.worker_id, j.lease_token, j.lease_expires_at,
		j.result, j.error, j.created_at, j.updated_at, j.started_at, j.finished_at,
		t.id, t.project_slug, t.telegram_chat_id, t.telegram_topic_id, t.title, t.codex_thread_id, t.created_at, t.updated_at,
		COALESCE(m.cwd, '')
	FROM codex_jobs j JOIN codex_threads t ON t.id = j.thread_id
	LEFT JOIN codex_thread_mirrors m ON m.thread_id = t.id`

type rowScanner interface{ Scan(...any) error }

func scanCodexJob(row rowScanner) (CodexJob, error) {
	var j CodexJob
	var leaseExpires, created, updated, started, finished, threadCreated, threadUpdated string
	err := row.Scan(&j.ID, &j.ThreadID, &j.Prompt, &j.Status, &j.WorkerID, &j.LeaseToken, &leaseExpires,
		&j.Result, &j.Error, &created, &updated, &started, &finished,
		&j.Thread.ID, &j.Thread.ProjectSlug, &j.Thread.TelegramChatID, &j.Thread.TelegramTopicID, &j.Thread.Title, &j.Thread.CodexThreadID, &threadCreated, &threadUpdated,
		&j.Thread.CWD)
	if err != nil {
		return CodexJob{}, fmt.Errorf("scan Codex job: %w", err)
	}
	j.LeaseExpiresAt, j.CreatedAt, j.UpdatedAt = parseDBTime(leaseExpires), parseDBTime(created), parseDBTime(updated)
	j.StartedAt, j.FinishedAt = parseDBTime(started), parseDBTime(finished)
	j.Thread.CreatedAt, j.Thread.UpdatedAt = parseDBTime(threadCreated), parseDBTime(threadUpdated)
	return j, nil
}

func (s *Store) scanCodexThread(row rowScanner) (CodexThread, bool, error) {
	var t CodexThread
	var created, updated string
	err := row.Scan(&t.ID, &t.ProjectSlug, &t.TelegramChatID, &t.TelegramTopicID, &t.Title, &t.CodexThreadID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexThread{}, false, nil
	}
	if err != nil {
		return CodexThread{}, false, fmt.Errorf("scan Codex thread: %w", err)
	}
	t.CreatedAt, t.UpdatedAt = parseDBTime(created), parseDBTime(updated)
	return t, true, nil
}

func randomCodexToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate Codex token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
