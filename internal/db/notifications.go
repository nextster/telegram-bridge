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
// row is never automatically replayed after uncertain remote delivery.
func (s *Store) ReserveNotification(ctx context.Context, accountID, chatID int64, eventID, digest string) (NotificationReceipt, bool, error) {
	if accountID <= 0 {
		return NotificationReceipt{}, false, errors.New("notification account is required")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO notification_receipts
		(account_id, chat_id, event_id, digest, status, created_at) VALUES (?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP)
		ON CONFLICT(account_id, chat_id, event_id) DO NOTHING`, accountID, chatID, eventID, digest)
	if err != nil {
		return NotificationReceipt{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return NotificationReceipt{}, false, err
	}
	var receipt NotificationReceipt
	var previousDigest string
	err = s.db.QueryRowContext(ctx, `SELECT digest, status, message_id FROM notification_receipts WHERE account_id=? AND chat_id=? AND event_id=?`, accountID, chatID, eventID).Scan(&previousDigest, &receipt.Status, &receipt.MessageID)
	if err != nil {
		return NotificationReceipt{}, false, err
	}
	if previousDigest != digest {
		return NotificationReceipt{}, false, errors.New("event_id already belongs to different notification text")
	}
	return receipt, count == 1, nil
}

func (s *Store) CompleteNotification(ctx context.Context, accountID, chatID int64, eventID string, messageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_receipts SET status='sent', message_id=? WHERE account_id=? AND chat_id=? AND event_id=? AND status='pending'`, messageID, accountID, chatID, eventID)
	return err
}
