package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type PrivateDialog struct {
	OwnerUserID    int64
	PeerID         int64
	AccessHash     int64
	Title          string
	Username       string
	IsBot          bool
	DiscoveredAt   time.Time
	UpdatedAt      time.Time
	LastBackfillAt time.Time
}

// PrivateMessage is a text/metadata snapshot. Media bytes are never archived.
type PrivateMessage struct {
	OwnerUserID int64
	MessageID   int
	PeerID      int64
	SenderID    int64
	MessageDate time.Time
	EditDate    time.Time
	Text        string
	MediaType   string
	Outgoing    bool
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	DeletedAt   time.Time
}

type PrivateMessageDeletion struct {
	Dialog     PrivateDialog
	Messages   []PrivateMessage
	ObservedAt time.Time
	Attempts   int
}

type PrivateArchiveStats struct {
	Dialogs  int
	Messages int
	Deleted  int
	Pending  int
}

func (s *Store) UpsertPrivateDialog(ctx context.Context, dialog PrivateDialog) error {
	if dialog.OwnerUserID <= 0 {
		return errors.New("private dialog owner user id is empty")
	}
	if dialog.PeerID <= 0 {
		return errors.New("private dialog peer id is empty")
	}
	now := time.Now().UTC()
	if dialog.DiscoveredAt.IsZero() {
		dialog.DiscoveredAt = now
	}
	if dialog.UpdatedAt.IsZero() {
		dialog.UpdatedAt = now
	}
	isBot := 0
	if dialog.IsBot {
		isBot = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO private_dialogs(owner_user_id, peer_id, access_hash, title, username, is_bot, discovered_at, updated_at, last_backfill_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, '')
		ON CONFLICT(owner_user_id, peer_id) DO UPDATE SET
			access_hash = CASE WHEN excluded.access_hash != 0 THEN excluded.access_hash ELSE private_dialogs.access_hash END,
			title = CASE WHEN excluded.title != '' THEN excluded.title ELSE private_dialogs.title END,
			username = CASE WHEN excluded.username != '' THEN excluded.username ELSE private_dialogs.username END,
			is_bot = excluded.is_bot,
			updated_at = excluded.updated_at
	`, dialog.OwnerUserID, dialog.PeerID, dialog.AccessHash, strings.TrimSpace(dialog.Title),
		strings.TrimPrefix(strings.TrimSpace(dialog.Username), "@"), isBot,
		formatTime(dialog.DiscoveredAt), formatTime(dialog.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert private dialog: %w", err)
	}
	return nil
}

func (s *Store) GetPrivateDialog(ctx context.Context, ownerUserID, peerID int64) (PrivateDialog, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT owner_user_id, peer_id, access_hash, title, username, is_bot, discovered_at, updated_at, last_backfill_at
		FROM private_dialogs
		WHERE owner_user_id = ? AND peer_id = ?
	`, ownerUserID, peerID)
	dialog, err := scanPrivateDialog(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PrivateDialog{}, false, nil
	}
	if err != nil {
		return PrivateDialog{}, false, err
	}
	return dialog, true, nil
}

func (s *Store) ListPrivateDialogsNeedingBackfill(ctx context.Context, ownerUserID int64, limit int) ([]PrivateDialog, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner_user_id, peer_id, access_hash, title, username, is_bot, discovered_at, updated_at, last_backfill_at
		FROM private_dialogs
		WHERE owner_user_id = ? AND is_bot = 0 AND peer_id != owner_user_id
		  AND access_hash != 0 AND last_backfill_at = ''
		ORDER BY updated_at DESC
		LIMIT ?
	`, ownerUserID, limit)
	if err != nil {
		return nil, fmt.Errorf("list private dialogs needing backfill: %w", err)
	}
	defer rows.Close()

	var out []PrivateDialog
	for rows.Next() {
		dialog, err := scanPrivateDialog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dialog)
	}
	return out, rows.Err()
}

func (s *Store) SetPrivateDialogBackfilled(ctx context.Context, ownerUserID, peerID int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE private_dialogs
		SET last_backfill_at = ?, updated_at = ?
		WHERE owner_user_id = ? AND peer_id = ?
	`, formatTime(at), nowText(), ownerUserID, peerID)
	if err != nil {
		return fmt.Errorf("set private dialog backfilled: %w", err)
	}
	return nil
}

func (s *Store) UpsertPrivateMessage(ctx context.Context, message PrivateMessage) error {
	if message.OwnerUserID <= 0 {
		return errors.New("private message owner user id is empty")
	}
	if message.MessageID <= 0 {
		return errors.New("private message id is empty")
	}
	if message.PeerID <= 0 {
		return errors.New("private message peer id is empty")
	}
	now := time.Now().UTC()
	if message.MessageDate.IsZero() {
		message.MessageDate = now
	}
	if message.FirstSeenAt.IsZero() {
		message.FirstSeenAt = now
	}
	if message.LastSeenAt.IsZero() {
		message.LastSeenAt = now
	}
	revisionDate := message.EditDate
	if revisionDate.IsZero() {
		revisionDate = message.MessageDate
	}
	outgoing := 0
	if message.Outgoing {
		outgoing = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO private_messages(
			owner_user_id, message_id, peer_id, sender_id, message_date, edit_date, revision_date,
			text, media_type, outgoing, first_seen_at, last_seen_at, deleted_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			COALESCE((SELECT observed_at FROM private_message_deletions WHERE owner_user_id = ? AND message_id = ?), ''))
			ON CONFLICT(owner_user_id, message_id) DO UPDATE SET
				edit_date = CASE
					WHEN private_messages.deleted_at = '' AND (
						excluded.revision_date > private_messages.revision_date
						OR (excluded.revision_date = private_messages.revision_date
							AND NOT (private_messages.edit_date != '' AND excluded.edit_date = ''))
					) THEN excluded.edit_date
					ELSE private_messages.edit_date END,
				revision_date = CASE
					WHEN private_messages.deleted_at = '' AND (
						excluded.revision_date > private_messages.revision_date
						OR (excluded.revision_date = private_messages.revision_date
							AND NOT (private_messages.edit_date != '' AND excluded.edit_date = ''))
					) THEN excluded.revision_date
					ELSE private_messages.revision_date END,
				text = CASE
					WHEN private_messages.deleted_at = '' AND (
						excluded.revision_date > private_messages.revision_date
						OR (excluded.revision_date = private_messages.revision_date
							AND NOT (private_messages.edit_date != '' AND excluded.edit_date = ''))
					) THEN excluded.text
					ELSE private_messages.text END,
				media_type = CASE
					WHEN private_messages.deleted_at = '' AND (
						excluded.revision_date > private_messages.revision_date
						OR (excluded.revision_date = private_messages.revision_date
							AND NOT (private_messages.edit_date != '' AND excluded.edit_date = ''))
					) THEN excluded.media_type
				ELSE private_messages.media_type END,
			last_seen_at = CASE
				WHEN excluded.last_seen_at > private_messages.last_seen_at THEN excluded.last_seen_at
				ELSE private_messages.last_seen_at END
	`, message.OwnerUserID, message.MessageID, message.PeerID, message.SenderID,
		formatTime(message.MessageDate), formatOptionalTime(message.EditDate), formatTime(revisionDate),
		message.Text, message.MediaType, outgoing, formatTime(message.FirstSeenAt), formatTime(message.LastSeenAt),
		message.OwnerUserID, message.MessageID)
	if err != nil {
		return fmt.Errorf("upsert private message: %w", err)
	}
	return nil
}

// RecordPrivateMessageDeletions stores tombstones even when a message snapshot
// has not arrived yet. This makes startup/backfill ordering safe and provides a
// durable notification outbox.
func (s *Store) RecordPrivateMessageDeletions(ctx context.Context, ownerUserID int64, messageIDs []int, observedAt time.Time) error {
	if ownerUserID <= 0 {
		return errors.New("private deletion owner user id is empty")
	}
	ids := uniquePositiveInts(messageIDs)
	if len(ids) == 0 {
		return nil
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin private message deletion: %w", err)
	}
	defer tx.Rollback()
	for _, messageID := range ids {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO private_message_deletions(owner_user_id, message_id, observed_at)
			VALUES(?, ?, ?)
		`, ownerUserID, messageID, formatTime(observedAt)); err != nil {
			return fmt.Errorf("record private message deletion: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE private_messages
			SET deleted_at = CASE WHEN deleted_at = '' THEN ? ELSE deleted_at END
			WHERE owner_user_id = ? AND message_id = ?
		`, formatTime(observedAt), ownerUserID, messageID); err != nil {
			return fmt.Errorf("mark private message deleted: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit private message deletion: %w", err)
	}
	return nil
}

func (s *Store) ListPendingPrivateMessageDeletions(ctx context.Context, ownerUserID int64, limit int) ([]PrivateMessageDeletion, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.observed_at, d.attempts,
		       p.owner_user_id, p.peer_id, p.access_hash, p.title, p.username, p.is_bot,
		       p.discovered_at, p.updated_at, p.last_backfill_at,
		       m.owner_user_id, m.message_id, m.peer_id, m.sender_id, m.message_date, m.edit_date,
		       m.text, m.media_type, m.outgoing, m.first_seen_at, m.last_seen_at, m.deleted_at
		FROM private_message_deletions d
		JOIN private_messages m
		  ON m.owner_user_id = d.owner_user_id AND m.message_id = d.message_id
		JOIN private_dialogs p
		  ON p.owner_user_id = m.owner_user_id AND p.peer_id = m.peer_id
		WHERE d.owner_user_id = ? AND d.notified_at = '' AND p.is_bot = 0
		  AND (d.next_attempt_at = '' OR d.next_attempt_at <= ?)
		ORDER BY d.observed_at, m.peer_id, m.message_date, m.message_id
		LIMIT ?
	`, ownerUserID, nowText(), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending private deletions: %w", err)
	}
	defer rows.Close()

	byPeer := make(map[int64]*PrivateMessageDeletion)
	var peerOrder []int64
	for rows.Next() {
		var observedAt string
		var attempts int
		var dialog PrivateDialog
		var isBot int
		var discoveredAt, updatedAt, lastBackfillAt string
		var message PrivateMessage
		var outgoing int
		var messageDate, editDate, firstSeenAt, lastSeenAt, deletedAt string
		if err := rows.Scan(&observedAt, &attempts,
			&dialog.OwnerUserID, &dialog.PeerID, &dialog.AccessHash, &dialog.Title, &dialog.Username, &isBot,
			&discoveredAt, &updatedAt, &lastBackfillAt,
			&message.OwnerUserID, &message.MessageID, &message.PeerID, &message.SenderID, &messageDate, &editDate,
			&message.Text, &message.MediaType, &outgoing, &firstSeenAt, &lastSeenAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("scan pending private deletion: %w", err)
		}
		dialog.IsBot = isBot == 1
		dialog.DiscoveredAt = parseDBTime(discoveredAt)
		dialog.UpdatedAt = parseDBTime(updatedAt)
		dialog.LastBackfillAt = parseDBTime(lastBackfillAt)
		message.Outgoing = outgoing == 1
		message.MessageDate = parseDBTime(messageDate)
		message.EditDate = parseDBTime(editDate)
		message.FirstSeenAt = parseDBTime(firstSeenAt)
		message.LastSeenAt = parseDBTime(lastSeenAt)
		message.DeletedAt = parseDBTime(deletedAt)

		batch := byPeer[message.PeerID]
		if batch == nil {
			batch = &PrivateMessageDeletion{Dialog: dialog, ObservedAt: parseDBTime(observedAt), Attempts: attempts}
			byPeer[message.PeerID] = batch
			peerOrder = append(peerOrder, message.PeerID)
		}
		if attempts > batch.Attempts {
			batch.Attempts = attempts
		}
		batch.Messages = append(batch.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending private deletions: %w", err)
	}
	out := make([]PrivateMessageDeletion, 0, len(peerOrder))
	for _, peerID := range peerOrder {
		out = append(out, *byPeer[peerID])
	}
	return out, nil
}

func (s *Store) MarkPrivateDeletionNotified(ctx context.Context, ownerUserID int64, messageIDs []int, at time.Time) error {
	return s.updatePrivateDeletionDelivery(ctx, ownerUserID, messageIDs, at, time.Time{}, "")
}

func (s *Store) MarkPrivateDeletionFailed(ctx context.Context, ownerUserID int64, messageIDs []int, at, retryAt time.Time, failure string) error {
	return s.updatePrivateDeletionDelivery(ctx, ownerUserID, messageIDs, at, retryAt, strings.TrimSpace(failure))
}

func (s *Store) updatePrivateDeletionDelivery(ctx context.Context, ownerUserID int64, messageIDs []int, at, retryAt time.Time, failure string) error {
	ids := uniquePositiveInts(messageIDs)
	if len(ids) == 0 {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if retryAt.IsZero() {
		retryAt = at
	}
	args := make([]any, 0, len(ids)+4)
	if failure == "" {
		args = append(args, formatTime(at), formatTime(at), ownerUserID)
	} else {
		args = append(args, formatTime(at), formatTime(retryAt), truncateDBError(failure), ownerUserID)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	query := `UPDATE private_message_deletions SET `
	if failure == "" {
		query += `notified_at = ?, attempts = attempts + 1, last_attempt_at = ?, next_attempt_at = '', last_error = '' `
	} else {
		query += `attempts = attempts + 1, last_attempt_at = ?, next_attempt_at = ?, last_error = ? `
	}
	query += `WHERE owner_user_id = ? AND message_id IN (` + sqlPlaceholders(len(ids)) + `)`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update private deletion delivery: %w", err)
	}
	return nil
}

func (s *Store) PrivateArchiveStats(ctx context.Context, ownerUserID int64) (PrivateArchiveStats, error) {
	var stats PrivateArchiveStats
	queries := []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM private_dialogs WHERE owner_user_id = ?`, &stats.Dialogs},
		{`SELECT COUNT(*) FROM private_messages WHERE owner_user_id = ?`, &stats.Messages},
		{`SELECT COUNT(*) FROM private_messages WHERE owner_user_id = ? AND deleted_at != ''`, &stats.Deleted},
		{`SELECT COUNT(*) FROM private_message_deletions WHERE owner_user_id = ? AND notified_at = ''`, &stats.Pending},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query, ownerUserID).Scan(item.dest); err != nil {
			return PrivateArchiveStats{}, fmt.Errorf("private archive stats: %w", err)
		}
	}
	return stats, nil
}

func (s *Store) PrunePrivateArchive(ctx context.Context, before time.Time, maxMessages int) error {
	if before.IsZero() {
		return nil
	}
	if maxMessages <= 0 {
		return errors.New("private archive message limit must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin private archive prune: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM private_messages
		WHERE message_date < ?
		  AND NOT EXISTS (
			SELECT 1 FROM private_message_deletions d
			WHERE d.owner_user_id = private_messages.owner_user_id
			  AND d.message_id = private_messages.message_id
			  AND d.notified_at = ''
		  )
	`, formatTime(before)); err != nil {
		return fmt.Errorf("prune private messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM private_messages
		WHERE (owner_user_id, message_id) IN (
			SELECT owner_user_id, message_id
			FROM private_messages m
			WHERE NOT EXISTS (
				SELECT 1 FROM private_message_deletions d
				WHERE d.owner_user_id = m.owner_user_id
				  AND d.message_id = m.message_id
				  AND d.notified_at = ''
			)
			ORDER BY message_date DESC, owner_user_id, message_id DESC
			LIMIT -1 OFFSET ?
		)
	`, maxMessages); err != nil {
		return fmt.Errorf("cap private messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM private_message_deletions
		WHERE observed_at < ?
		  AND (
			notified_at != ''
			OR NOT EXISTS (
				SELECT 1 FROM private_messages m
				WHERE m.owner_user_id = private_message_deletions.owner_user_id
				  AND m.message_id = private_message_deletions.message_id
			)
		  )
	`, formatTime(before)); err != nil {
		return fmt.Errorf("prune private deletions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit private archive prune: %w", err)
	}
	return nil
}

type privateDialogScanner interface{ Scan(dest ...any) error }

func scanPrivateDialog(scanner privateDialogScanner) (PrivateDialog, error) {
	var dialog PrivateDialog
	var isBot int
	var discoveredAt, updatedAt, lastBackfillAt string
	if err := scanner.Scan(&dialog.OwnerUserID, &dialog.PeerID, &dialog.AccessHash, &dialog.Title, &dialog.Username, &isBot,
		&discoveredAt, &updatedAt, &lastBackfillAt); err != nil {
		return PrivateDialog{}, err
	}
	dialog.IsBot = isBot == 1
	dialog.DiscoveredAt = parseDBTime(discoveredAt)
	dialog.UpdatedAt = parseDBTime(updatedAt)
	dialog.LastBackfillAt = parseDBTime(lastBackfillAt)
	return dialog, nil
}

func uniquePositiveInts(values []int) []int {
	seen := make(map[int]struct{}, len(values))
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return formatTime(value)
}

func truncateDBError(value string) string {
	const max = 1000
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}
