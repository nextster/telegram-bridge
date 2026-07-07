package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/nextster/tg-radar/internal/db"
)

type captureNotifier struct {
	events []db.Event
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
