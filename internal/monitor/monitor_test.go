package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/nextster/tg-radar/internal/db"
)

type captureNotifier struct {
	events      []db.Event
	deletions   []db.PrivateMessageDeletion
	failDeletes int
}

func (c *captureNotifier) NotifyDeletedMessages(_ context.Context, deletion db.PrivateMessageDeletion) error {
	if c.failDeletes > 0 {
		c.failDeletes--
		return errors.New("temporary send failure")
	}
	c.deletions = append(c.deletions, deletion)
	return nil
}

func (c *captureNotifier) NotifyEvent(_ context.Context, event db.Event) error {
	c.events = append(c.events, event)
	return nil
}

func TestHandlerRecordsAndNotifiesKeywordMatchOnce(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/tg-radar.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.AddKeyword(ctx, "radar"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMonitorPeer(ctx, db.MonitorPeer{
		PeerType:   "channel",
		PeerID:     1001,
		AccessHash: 123,
		Title:      "Radar Channel",
		Kind:       "channel",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorPeerEnabled(ctx, "channel", 1001, true); err != nil {
		t.Fatal(err)
	}

	notifier := &captureNotifier{}
	handler := NewHandler(store, notifier)
	update := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateNewChannelMessage{
				Message: &tg.Message{
					ID:      42,
					PeerID:  &tg.PeerChannel{ChannelID: 1001},
					Date:    int(time.Now().Unix()),
					Message: "new radar signal",
				},
			},
		},
	}

	if err := handler.Handle(ctx, update); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(ctx, update); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(events); got != 1 {
		t.Fatalf("events len = %d, want 1", got)
	}
	if events[0].Keyword != "radar" {
		t.Fatalf("keyword = %q, want radar", events[0].Keyword)
	}
	if events[0].SourcePeerType != "channel" || events[0].SourcePeerID != 1001 {
		t.Fatalf("source = %s:%d, want channel:1001", events[0].SourcePeerType, events[0].SourcePeerID)
	}
	if got := len(notifier.events); got != 1 {
		t.Fatalf("notified events len = %d, want 1", got)
	}
}

func TestBackfillFloodWait(t *testing.T) {
	wait, ok := backfillFloodWait(tgerr.New(420, "FLOOD_WAIT_29"))
	if !ok {
		t.Fatal("expected FLOOD_WAIT to be recognized")
	}
	if wait != 30*time.Second {
		t.Fatalf("wait = %s, want 30s", wait)
	}

	if _, ok := backfillFloodWait(tgerr.New(400, "PHONE_CODE_INVALID")); ok {
		t.Fatal("non-flood error recognized as flood wait")
	}
}

func TestHandlerArchivesEditedPrivateMessageAndNotifiesDeletionOnce(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/private-deletions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	notifier := &captureNotifier{}
	handler := NewHandler(store, notifier)
	handler.SetSelfUserID(100)
	peer := &tg.PeerUser{UserID: 200}
	from := &tg.PeerUser{UserID: 200}
	users := []tg.UserClass{&tg.User{ID: 200, AccessHash: 300, FirstName: "Alice", LastName: "Example", Username: "alice"}}

	if err := handler.Handle(ctx, &tg.Updates{
		Users: users,
		Updates: []tg.UpdateClass{&tg.UpdateNewMessage{Message: &tg.Message{
			ID: 77, PeerID: peer, FromID: from, Date: 100, Message: "original private text",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(ctx, &tg.Updates{
		Users: users,
		Updates: []tg.UpdateClass{&tg.UpdateEditMessage{Message: &tg.Message{
			ID: 77, PeerID: peer, FromID: from, Date: 100, EditDate: 110, Message: "edited private text",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	deletion := &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: []int{77}}}}
	if err := handler.Handle(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	if err := handler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	if err := handler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}

	if len(notifier.events) != 0 {
		t.Fatalf("private non-match created %d radar events", len(notifier.events))
	}
	if len(notifier.deletions) != 1 {
		t.Fatalf("deletion notifications = %d, want 1", len(notifier.deletions))
	}
	got := notifier.deletions[0]
	if got.Dialog.Title != "Alice Example" || got.Dialog.Username != "alice" {
		t.Fatalf("deletion dialog = %#v", got.Dialog)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "edited private text" || got.Messages[0].MessageID != 77 {
		t.Fatalf("deleted messages = %#v", got.Messages)
	}
}

func TestPrivateDeletionOutboxRetriesFailedNotification(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/private-deletion-retry.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	notifier := &captureNotifier{failDeletes: 1}
	handler := NewHandler(store, notifier)
	handler.SetSelfUserID(100)
	users := []tg.UserClass{&tg.User{ID: 200, AccessHash: 300, FirstName: "Alice"}}
	if err := handler.Handle(ctx, &tg.Updates{Users: users, Updates: []tg.UpdateClass{
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 88, PeerID: &tg.PeerUser{UserID: 200}, FromID: &tg.PeerUser{UserID: 200}, Date: 100, Message: "retry me"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: []int{88}}}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.flushPrivateDeletionOutbox(ctx); err == nil {
		t.Fatal("first flush unexpectedly succeeded")
	}
	if len(notifier.deletions) != 0 {
		t.Fatalf("failed attempt delivered %d notifications", len(notifier.deletions))
	}
	if err := store.MarkPrivateDeletionFailed(ctx, 100, []int{88}, time.Now().UTC(), time.Now().Add(-time.Second), "ready to retry"); err != nil {
		t.Fatal(err)
	}
	if err := handler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notifier.deletions) != 1 {
		t.Fatalf("retried notifications = %d, want 1", len(notifier.deletions))
	}
}

func TestPrivateDeletionOutboxChunksAndNopDoesNotAcknowledge(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/private-deletion-chunks.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	notifier := &captureNotifier{}
	handler := NewHandler(store, notifier)
	handler.SetSelfUserID(100)
	users := []tg.UserClass{&tg.User{ID: 200, AccessHash: 300, FirstName: "Alice"}}
	updates := make([]tg.UpdateClass, 0, 8)
	ids := make([]int, 0, 8)
	for id := 1; id <= 8; id++ {
		ids = append(ids, id)
		updates = append(updates, &tg.UpdateNewMessage{Message: &tg.Message{
			ID: id, PeerID: &tg.PeerUser{UserID: 200}, FromID: &tg.PeerUser{UserID: 200}, Date: 100 + id, Message: "chunk me",
		}})
	}
	if err := handler.Handle(ctx, &tg.Updates{Users: users, Updates: updates}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: ids}}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notifier.deletions) != 2 || len(notifier.deletions[0].Messages) != 6 || len(notifier.deletions[1].Messages) != 2 {
		t.Fatalf("deletion chunks = %#v", notifier.deletions)
	}

	if err := handler.Handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: []int{999}}}}); err != nil {
		t.Fatal(err)
	}
	nopHandler := NewHandler(store, nil)
	nopHandler.SetSelfUserID(100)
	if err := nopHandler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := store.PrivateArchiveStats(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending after nop flush = %d, want 1", stats.Pending)
	}
}
