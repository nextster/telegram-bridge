package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// PrivateMediaTTL is how long an attachment from a direct chat stays on
	// disk after it was downloaded.
	PrivateMediaTTL = time.Hour
	// PrivateMediaDeletedHold keeps the attachment of a deleted message until
	// the owner has had a chance to see it.
	PrivateMediaDeletedHold = 24 * time.Hour
)

// PrivateMedia describes one cached attachment of a direct-chat message. The
// file lives on disk until BotFileID is known; after that the bot resends it
// by that ID and the local copy is gone.
type PrivateMedia struct {
	OwnerUserID int64
	MessageID   int
	MediaID     int64
	Kind        string
	FileName    string
	MIME        string
	Size        int64
	Duration    int
	Width       int
	Height      int
	BotFileID   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// SavePrivateMedia records a downloaded attachment. If the message was
// already deleted, the attachment is held like any other deleted one.
func (s *Store) SavePrivateMedia(ctx context.Context, media PrivateMedia) error {
	if media.OwnerUserID <= 0 || media.MessageID <= 0 {
		return errors.New("private media owner and message are required")
	}
	if strings.TrimSpace(media.Kind) == "" || media.Size <= 0 {
		return errors.New("private media kind and size are required")
	}
	if media.CreatedAt.IsZero() {
		media.CreatedAt = time.Now().UTC()
	}
	if media.ExpiresAt.IsZero() {
		media.ExpiresAt = media.CreatedAt.Add(PrivateMediaTTL)
	}
	heldUntil := media.CreatedAt.Add(PrivateMediaDeletedHold).Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO private_media_cache(
			owner_user_id, message_id, media_id, kind, file_name, mime_type, size_bytes,
			duration_seconds, width, height, bot_file_id, created_at, expires_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?,
			CASE WHEN EXISTS(
				SELECT 1 FROM private_message_deletions WHERE owner_user_id = ? AND message_id = ?
			) THEN MAX(?, ?) ELSE ? END)
		ON CONFLICT(owner_user_id, message_id) DO UPDATE SET
			media_id = excluded.media_id,
			kind = excluded.kind,
			file_name = excluded.file_name,
			mime_type = excluded.mime_type,
			size_bytes = excluded.size_bytes,
			duration_seconds = excluded.duration_seconds,
			width = excluded.width,
			height = excluded.height,
			bot_file_id = '',
			created_at = excluded.created_at,
			expires_at = excluded.expires_at
	`, media.OwnerUserID, media.MessageID, media.MediaID, media.Kind, media.FileName, media.MIME, media.Size,
		media.Duration, media.Width, media.Height, media.CreatedAt.Unix(),
		media.OwnerUserID, media.MessageID, media.ExpiresAt.Unix(), heldUntil, media.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("save private media: %w", err)
	}
	return nil
}

// GetPrivateMedia returns the unexpired cached attachment of one message.
func (s *Store) GetPrivateMedia(ctx context.Context, ownerUserID int64, messageID int, now time.Time) (PrivateMedia, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+privateMediaColumns+`
		FROM private_media_cache
		WHERE owner_user_id = ? AND message_id = ? AND expires_at > ?
	`, ownerUserID, messageID, now.Unix())
	media, err := scanPrivateMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PrivateMedia{}, false, nil
	}
	if err != nil {
		return PrivateMedia{}, false, err
	}
	return media, true, nil
}

// SetPrivateMediaBotFileID remembers the Bot API file ID after the bot has
// uploaded the attachment once.
func (s *Store) SetPrivateMediaBotFileID(ctx context.Context, ownerUserID int64, messageID int, fileID string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE private_media_cache SET bot_file_id = ? WHERE owner_user_id = ? AND message_id = ?
	`, strings.TrimSpace(fileID), ownerUserID, messageID); err != nil {
		return fmt.Errorf("set private media bot file id: %w", err)
	}
	return nil
}

func (s *Store) DeletePrivateMedia(ctx context.Context, ownerUserID int64, messageID int) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM private_media_cache WHERE owner_user_id = ? AND message_id = ?
	`, ownerUserID, messageID); err != nil {
		return fmt.Errorf("delete private media: %w", err)
	}
	return nil
}

// ListExpiredPrivateMedia returns cache rows whose time is up.
func (s *Store) ListExpiredPrivateMedia(ctx context.Context, now time.Time, limit int) ([]PrivateMedia, error) {
	return s.listPrivateMedia(ctx, `WHERE expires_at <= ? ORDER BY expires_at LIMIT ?`, now.Unix(), limit)
}

// ListLocalPrivateMedia returns every row whose file is still on disk.
func (s *Store) ListLocalPrivateMedia(ctx context.Context) ([]PrivateMedia, error) {
	return s.listPrivateMedia(ctx, `WHERE bot_file_id = '' ORDER BY created_at`)
}

// ListEvictablePrivateMedia returns local files of messages that were not
// deleted, oldest first. They are the first to go when the cache is full.
func (s *Store) ListEvictablePrivateMedia(ctx context.Context, limit int) ([]PrivateMedia, error) {
	return s.listPrivateMedia(ctx, `
		WHERE bot_file_id = '' AND NOT EXISTS (
			SELECT 1 FROM private_message_deletions d
			WHERE d.owner_user_id = private_media_cache.owner_user_id
			  AND d.message_id = private_media_cache.message_id
		)
		ORDER BY created_at LIMIT ?`, limit)
}

func (s *Store) listPrivateMedia(ctx context.Context, where string, args ...any) ([]PrivateMedia, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+privateMediaColumns+` FROM private_media_cache `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list private media: %w", err)
	}
	defer rows.Close()
	var out []PrivateMedia
	for rows.Next() {
		media, err := scanPrivateMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, media)
	}
	return out, rows.Err()
}

const privateMediaColumns = `owner_user_id, message_id, media_id, kind, file_name, mime_type, size_bytes,
	duration_seconds, width, height, bot_file_id, created_at, expires_at`

func scanPrivateMedia(scanner interface{ Scan(dest ...any) error }) (PrivateMedia, error) {
	var media PrivateMedia
	var createdAt, expiresAt int64
	if err := scanner.Scan(&media.OwnerUserID, &media.MessageID, &media.MediaID, &media.Kind, &media.FileName, &media.MIME,
		&media.Size, &media.Duration, &media.Width, &media.Height, &media.BotFileID, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PrivateMedia{}, err
		}
		return PrivateMedia{}, fmt.Errorf("scan private media: %w", err)
	}
	media.CreatedAt = time.Unix(createdAt, 0).UTC()
	media.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return media, nil
}
