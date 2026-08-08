package notify

import (
	"context"

	"github.com/nextster/tg-radar/internal/db"
)

type Notifier interface {
	NotifyEvent(ctx context.Context, event db.Event) error
}

type DeletionNotifier interface {
	NotifyDeletedMessages(ctx context.Context, deletion db.PrivateMessageDeletion) error
}

type SystemNotifier interface {
	NotifySystem(ctx context.Context, text string) error
}

type Nop struct{}

func (Nop) NotifyEvent(context.Context, db.Event) error {
	return nil
}

func (Nop) NotifyDeletedMessages(context.Context, db.PrivateMessageDeletion) error {
	return nil
}

func (Nop) NotifySystem(context.Context, string) error {
	return nil
}
