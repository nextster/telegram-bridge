package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestOpenEnablesForeignKeysOnEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/foreign-keys.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connections := make([]*sql.Conn, 0, 4)
	defer func() {
		for _, connection := range connections {
			connection.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		connection, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		var enabled int
		if err := connection.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 {
			t.Fatalf("connection %d foreign_keys = %d, want 1", i, enabled)
		}
	}
}

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

func TestPrivateMessageSnapshotEditAndDeletionAreIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/private-messages.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	firstSeen := time.Unix(100, 0).UTC()
	if err := store.UpsertPrivateDialog(ctx, PrivateDialog{
		OwnerUserID: 100, PeerID: 200, AccessHash: 300, Title: "Alice Example", Username: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 42, PeerID: 200, SenderID: 200, MessageDate: time.Unix(90, 0).UTC(),
		Text: "before edit", FirstSeenAt: firstSeen, LastSeenAt: firstSeen,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 42, PeerID: 200, SenderID: 200, MessageDate: time.Unix(90, 0).UTC(),
		EditDate: time.Unix(110, 0).UTC(), Text: "after edit", LastSeenAt: time.Unix(110, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 42, PeerID: 200, SenderID: 200, MessageDate: time.Unix(90, 0).UTC(),
		Text: "stale original", LastSeenAt: time.Unix(115, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 44, PeerID: 200, SenderID: 200, MessageDate: time.Unix(200, 0).UTC(),
		Text: "same-second original",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 44, PeerID: 200, SenderID: 200, MessageDate: time.Unix(200, 0).UTC(),
		EditDate: time.Unix(200, 0).UTC(), Text: "same-second edit",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 44, PeerID: 200, SenderID: 200, MessageDate: time.Unix(200, 0).UTC(),
		Text: "same-second stale replay",
	}); err != nil {
		t.Fatal(err)
	}

	deletedAt := time.Unix(120, 0).UTC()
	if err := store.RecordPrivateMessageDeletions(ctx, 100, []int{42, 42, 999}, deletedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 42, PeerID: 200, SenderID: 200, MessageDate: time.Unix(90, 0).UTC(),
		EditDate: time.Unix(130, 0).UTC(), Text: "edit after deletion must not replace snapshot",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingPrivateMessageDeletions(ctx, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || len(pending[0].Messages) != 1 {
		t.Fatalf("pending deletions = %#v", pending)
	}
	got := pending[0].Messages[0]
	if got.Text != "after edit" || got.PeerID != 200 || got.SenderID != 200 {
		t.Fatalf("deleted message = %#v", got)
	}
	if !got.FirstSeenAt.Equal(firstSeen) || !got.EditDate.Equal(time.Unix(110, 0).UTC()) || !got.DeletedAt.Equal(deletedAt) {
		t.Fatalf("deleted message dates = %#v", got)
	}

	if err := store.MarkPrivateDeletionNotified(ctx, 100, []int{42}, deletedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPrivateMessageDeletions(ctx, 100, []int{42}, deletedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingPrivateMessageDeletions(ctx, 100, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("duplicate deletion pending=%#v err=%v", pending, err)
	}

	dialog, ok, err := store.GetPrivateDialog(ctx, 100, 200)
	if err != nil || !ok {
		t.Fatalf("get private dialog: ok=%v err=%v", ok, err)
	}
	if dialog.Title != "Alice Example" || dialog.Username != "alice" {
		t.Fatalf("private dialog = %#v", dialog)
	}

	if err := store.RecordPrivateMessageDeletions(ctx, 100, []int{43}, deletedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPrivateMessage(ctx, PrivateMessage{
		OwnerUserID: 100, MessageID: 43, PeerID: 200, SenderID: 200,
		MessageDate: time.Unix(119, 0).UTC(), Text: "snapshot arrived after tombstone",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingPrivateMessageDeletions(ctx, 100, 10)
	if err != nil || len(pending) != 1 || len(pending[0].Messages) != 1 || pending[0].Messages[0].MessageID != 43 {
		t.Fatalf("tombstone-first pending=%#v err=%v", pending, err)
	}
	if err := store.RecordPrivateMessageDeletions(ctx, 100, []int{44}, deletedAt); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingPrivateMessageDeletions(ctx, 100, 10)
	if err != nil || len(pending) != 1 || pending[0].Messages[len(pending[0].Messages)-1].Text != "same-second edit" {
		t.Fatalf("same-second edit regressed: pending=%#v err=%v", pending, err)
	}
}

func TestPrivateArchivePruneProtectsPendingDeletion(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/private-prune.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertPrivateDialog(ctx, PrivateDialog{OwnerUserID: 100, PeerID: 200, AccessHash: 300, Title: "Alice"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []PrivateMessage{
		{OwnerUserID: 100, MessageID: 1, PeerID: 200, MessageDate: time.Unix(10, 0).UTC(), Text: "pending old message"},
		{OwnerUserID: 100, MessageID: 2, PeerID: 200, MessageDate: time.Unix(20, 0).UTC(), Text: "ordinary old message"},
	} {
		if err := store.UpsertPrivateMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordPrivateMessageDeletions(ctx, 100, []int{1}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.PrunePrivateArchive(ctx, time.Unix(30, 0).UTC(), 1); err != nil {
		t.Fatal(err)
	}
	stats, err := store.PrivateArchiveStats(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 || stats.Pending != 1 {
		t.Fatalf("stats after protected prune = %#v", stats)
	}
	pending, err := store.ListPendingPrivateMessageDeletions(ctx, 100, 10)
	if err != nil || len(pending) != 1 || pending[0].Messages[0].Text != "pending old message" {
		t.Fatalf("pending after protected prune = %#v, err=%v", pending, err)
	}
	if err := store.MarkPrivateDeletionNotified(ctx, 100, []int{1}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.PrunePrivateArchive(ctx, time.Now().UTC(), 1); err != nil {
		t.Fatal(err)
	}
	stats, err = store.PrivateArchiveStats(ctx, 100)
	if err != nil || stats.Messages != 0 || stats.Pending != 0 {
		t.Fatalf("stats after delivered prune = %#v, err=%v", stats, err)
	}
}

func TestWatchRuleRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/rules.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rule, err := store.UpsertWatchRule(ctx, Keyword{
		Phrase:              "repair stand",
		AnyTerms:            []string{"workstand", "ремонтная стойка", "workstand"},
		AllTerms:            []string{"bike"},
		RequiredAnyGroups:   [][]string{{"folding", "складная"}, {"clamp", "зажим"}},
		PreferredTerms:      []string{"seatpost", "подседельный"},
		ExcludeTerms:        []string{"sold"},
		Note:                "Clamp the seatpost, never the carbon frame.",
		ExcludeCompleteBike: true,
		Sources:             []RuleSource{{PeerType: "channel", PeerID: 42}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rule.AnyTerms) != 2 || len(rule.AllTerms) != 1 || len(rule.RequiredAnyGroups) != 2 ||
		len(rule.PreferredTerms) != 2 || len(rule.ExcludeTerms) != 1 || rule.Note == "" ||
		!rule.ExcludeCompleteBike || len(rule.Sources) != 1 {
		t.Fatalf("unexpected rule: %#v", rule)
	}

	event, inserted, err := store.RecordEvent(ctx, Event{
		SourcePeerType: "channel", SourcePeerID: 42, MessageID: 7, Text: "bike workstand",
		Keyword: rule.Phrase, RuleID: rule.ID, MatchReason: "any: workstand; all: bike", MatchScore: 115,
	})
	if err != nil || !inserted {
		t.Fatalf("record event: inserted=%v err=%v", inserted, err)
	}
	loaded, ok, err := store.GetEvent(ctx, event.ID)
	if err != nil || !ok {
		t.Fatalf("get event: ok=%v err=%v", ok, err)
	}
	if loaded.RuleID != rule.ID || loaded.MatchScore != 115 || loaded.MatchReason == "" {
		t.Fatalf("unexpected event details: %#v", loaded)
	}
}

func TestApplyWatchRulesIsAtomicAndPreservesUnrelatedRules(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/rules.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.AddKeyword(ctx, "cargo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddKeyword(ctx, "кассета"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyWatchRules(ctx, []Keyword{
		{Phrase: "exact cassette", AnyTerms: []string{"xg-1250"}},
		{Phrase: "broken"},
	}, []string{"кассета"}); err == nil {
		t.Fatal("invalid batch succeeded")
	}
	if _, err := store.GetKeywordByPhrase(ctx, "кассета"); err != nil {
		t.Fatalf("old rule disappeared after rejected batch: %v", err)
	}

	deleted, err := store.ApplyWatchRules(ctx, []Keyword{{
		Phrase:            "exact cassette",
		AnyTerms:          []string{"cassette"},
		RequiredAnyGroups: [][]string{{"xg-1250"}, {"10-36"}, {"xdr"}},
		Note:              "Buy only after confirmed wear.",
	}}, []string{"кассета"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if _, err := store.GetKeywordByPhrase(ctx, "cargo"); err != nil {
		t.Fatalf("unrelated rule was not preserved: %v", err)
	}
	if _, err := store.GetKeywordByPhrase(ctx, "exact cassette"); err != nil {
		t.Fatalf("new rule missing: %v", err)
	}
}
