package db

import (
	"context"
	"errors"
)

type NotificationReceipt struct {
	Status    string `json:"status"`
	MessageID int    `json:"message_id,omitempty"`
}

// ReserveNotification commits intent before the remote side effect. A pending
// row is never automatically replayed because Bot API has no idempotency key.
func (s *Store) ReserveNotification(ctx context.Context, chatID int64, eventID, digest string) (NotificationReceipt, bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO notification_receipts
		(chat_id, event_id, digest, status, created_at) VALUES (?, ?, ?, 'pending', CURRENT_TIMESTAMP)
		ON CONFLICT(chat_id, event_id) DO NOTHING`, chatID, eventID, digest)
	if err != nil {
		return NotificationReceipt{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return NotificationReceipt{}, false, err
	}
	var receipt NotificationReceipt
	var previousDigest string
	err = s.db.QueryRowContext(ctx, `SELECT digest, status, message_id FROM notification_receipts WHERE chat_id=? AND event_id=?`, chatID, eventID).Scan(&previousDigest, &receipt.Status, &receipt.MessageID)
	if err != nil {
		return NotificationReceipt{}, false, err
	}
	if previousDigest != digest {
		return NotificationReceipt{}, false, errors.New("event_id already belongs to different notification text")
	}
	return receipt, count == 1, nil
}

func (s *Store) CompleteNotification(ctx context.Context, chatID int64, eventID string, messageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_receipts SET status='sent', message_id=? WHERE chat_id=? AND event_id=? AND status='pending'`, messageID, chatID, eventID)
	return err
}
