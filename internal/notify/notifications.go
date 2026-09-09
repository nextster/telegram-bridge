package notify

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nextster/telegram-bridge/internal/db"
)

type NotificationSender interface {
	CheckNotificationChat(context.Context, int64) error
	SendNotification(context.Context, int64, string, string) (int, error)
}

type NotificationInput struct {
	Chat    string `json:"chat"`
	EventID string `json:"event_id"`
	Text    string `json:"text"`
}

type Notifications struct {
	store   *db.Store
	sender  NotificationSender
	allowed map[int64]bool
}

func NewNotifications(store *db.Store, sender NotificationSender, chatIDs []int64) *Notifications {
	allowed := make(map[int64]bool, len(chatIDs))
	for _, id := range chatIDs {
		if id < 0 {
			allowed[id] = true
		}
	}
	return &Notifications{store: store, sender: sender, allowed: allowed}
}

var eventIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (n *Notifications) Send(ctx context.Context, input NotificationInput) (db.NotificationReceipt, error) {
	if n == nil || n.store == nil || n.sender == nil {
		return db.NotificationReceipt{}, errors.New("notifications are disabled")
	}
	chatID, err := notificationChatID(input.Chat)
	if err != nil || !n.allowed[chatID] {
		return db.NotificationReceipt{}, errors.New("notification group is not allowed")
	}
	if !eventIDPattern.MatchString(input.EventID) {
		return db.NotificationReceipt{}, errors.New("invalid notification event_id")
	}
	if strings.TrimSpace(input.Text) == "" || len(utf16.Encode([]rune(input.Text))) > 4096 {
		return db.NotificationReceipt{}, errors.New("notification text must contain 1-4096 UTF-16 code units")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := n.sender.CheckNotificationChat(ctx, chatID); err != nil {
		return db.NotificationReceipt{}, errors.New("notification group is unavailable to the authorized account")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(input.Text)))
	receipt, reserved, err := n.store.ReserveNotification(ctx, chatID, input.EventID, digest)
	if err != nil {
		return db.NotificationReceipt{}, err
	}
	if !reserved {
		if receipt.Status == "sent" {
			return receipt, nil
		}
		return db.NotificationReceipt{}, errors.New("notification delivery is pending or uncertain; inspect the group before recovery")
	}
	messageID, err := n.sender.SendNotification(ctx, chatID, input.Text, input.EventID)
	if err != nil || messageID <= 0 {
		return db.NotificationReceipt{}, errors.New("notification delivery is uncertain; no automatic retry; inspect the group")
	}
	// Persist an acknowledged send even if the HTTP caller disconnected.
	receiptCtx, receiptCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer receiptCancel()
	if err := n.store.CompleteNotification(receiptCtx, chatID, input.EventID, messageID); err != nil {
		return db.NotificationReceipt{}, errors.New("notification was sent but receipt storage failed; inspect the group")
	}
	return db.NotificationReceipt{Status: "sent", MessageID: messageID}, nil
}

func notificationChatID(key string) (int64, error) {
	kind, rawID, ok := strings.Cut(key, ":")
	if !ok {
		return 0, errors.New("invalid group key")
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rawID {
		return 0, errors.New("invalid group id")
	}
	switch {
	case kind == "chat" && id <= 999999999999:
		return -id, nil
	case kind == "channel" && id <= 997852516352:
		return -1000000000000 - id, nil
	default:
		return 0, errors.New("notifications require a group")
	}
}
