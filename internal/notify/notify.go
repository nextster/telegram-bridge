package notify

import (
	"context"

	"github.com/nextster/telegram-bridge/internal/db"
)

type Notifier interface {
	NotifyEvent(ctx context.Context, event db.Event) error
}

type DeletionNotifier interface {
	NotifyDeletedMessages(ctx context.Context, deletion db.PrivateMessageDeletion) error
}

type Nop struct{}

func (Nop) NotifyEvent(context.Context, db.Event) error {
	return nil
}

func (Nop) NotifyDeletedMessages(context.Context, db.PrivateMessageDeletion) error {
	return nil
}
