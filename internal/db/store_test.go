package db

import (
	"context"
	"testing"
	"time"
)

func TestGetEvent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/tg-radar.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	created, inserted, err := store.RecordEvent(ctx, Event{
		SourcePeerType: "channel",
		SourcePeerID:   1000000001,
		MessageID:      42,
		MessageDate:    time.Unix(100, 0).UTC(),
		Text:           "radar text",
		Keyword:        "radar",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("event was not inserted")
	}

	got, ok, err := store.GetEvent(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("event not found")
	}
	if got.ID != created.ID || got.Keyword != "radar" || got.SourcePeerID != 1000000001 || got.MessageID != 42 {
		t.Fatalf("event = %+v", got)
	}

	if _, ok, err := store.GetEvent(ctx, created.ID+1); err != nil || ok {
		t.Fatalf("missing event ok=%v err=%v", ok, err)
	}
}
