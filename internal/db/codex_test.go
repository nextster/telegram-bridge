package db

import (
	"context"
	"testing"
	"time"
)

func TestCodexJobLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/telegram-bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	project := CodexProject{Slug: "bridge", Title: "Bridge", TelegramChannelID: 42, TelegramAccessHash: 9, TelegramChatID: -1000000000042}
	if err := store.UpsertCodexProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	thread, err := store.CreateCodexThread(ctx, CodexThread{ProjectSlug: project.Slug, TelegramChatID: project.TelegramChatID, TelegramTopicID: 7, Title: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueCodexJob(ctx, thread.ID, "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimCodexJob(ctx, "mac", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claimed.ID != queued.ID || claimed.LeaseToken == "" || claimed.Thread.ProjectSlug != project.Slug {
		t.Fatalf("claimed = %+v", claimed)
	}
	if err := store.StartCodexJob(ctx, claimed.ID, claimed.LeaseToken, "codex-thread"); err != nil {
		t.Fatal(err)
	}
	finished, err := store.FinishCodexJob(ctx, claimed.ID, claimed.LeaseToken, "done", "")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "succeeded" || finished.Result != "done" || finished.Thread.CodexThreadID != "codex-thread" {
		t.Fatalf("finished = %+v", finished)
	}
	if err := store.SetCodexThreadID(ctx, thread.ID, "codex-thread-fork"); err != nil {
		t.Fatal(err)
	}
	archived, err := store.PendingArchivedCodexThreads(ctx, []string{"codex-thread"})
	if err != nil || len(archived) != 1 || archived[0].ID != thread.ID {
		t.Fatalf("archived=%+v err=%v", archived, err)
	}
	if err := store.MarkCodexTopicDeleted(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if archived, err := store.PendingArchivedCodexThreads(ctx, []string{"codex-thread-fork"}); err != nil || len(archived) != 0 {
		t.Fatalf("already deleted archived=%+v err=%v", archived, err)
	}
	if _, ok, err := store.ClaimCodexJob(ctx, "mac", time.Minute); err != nil || ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
}

func TestRetryFailedCodexJob(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/telegram-bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := CodexProject{Slug: "bridge", Title: "Bridge", TelegramChannelID: 42, TelegramAccessHash: 9, TelegramChatID: -1000000000042}
	if err := store.UpsertCodexProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	thread, err := store.CreateCodexThread(ctx, CodexThread{ProjectSlug: project.Slug, TelegramChatID: project.TelegramChatID, TelegramTopicID: 7, Title: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueCodexJob(ctx, thread.ID, "try again")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimCodexJob(ctx, "mac", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := store.FinishCodexJob(ctx, claimed.ID, claimed.LeaseToken, "", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryCodexJob(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	retried, ok, err := store.ClaimCodexJob(ctx, "mac", time.Minute)
	if err != nil || !ok || retried.ID != queued.ID || retried.Prompt != "try again" {
		t.Fatalf("retried=%+v ok=%v err=%v", retried, ok, err)
	}
}

func TestCodexReadReceiptsAndOutboundMessages(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/telegram-bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := CodexProject{Slug: "active", Title: "Active", TelegramChannelID: 42, TelegramAccessHash: 9, TelegramChatID: -1000000000042}
	if err := store.UpsertCodexProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	thread, err := store.CreateCodexThread(ctx, CodexThread{ProjectSlug: project.Slug, TelegramChatID: project.TelegramChatID, TelegramTopicID: 7, Title: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCodexThreadID(ctx, thread.ID, "thread-1"); err != nil {
		t.Fatal(err)
	}
	deliver, err := store.ObserveCodexTopicReadState(ctx, project.TelegramChatID, 7, false, 88)
	if err != nil || !deliver {
		t.Fatal(err)
	}
	ids, err := store.PendingCodexReadReceipts(ctx, 10)
	if err != nil || len(ids) != 1 || ids[0] != "thread-1" {
		t.Fatalf("receipts=%v err=%v", ids, err)
	}
	if err := store.AckCodexReadReceipts(ctx, ids); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.PendingCodexReadReceipts(ctx, 10); err != nil || len(ids) != 0 {
		t.Fatalf("acked receipts=%v err=%v", ids, err)
	}
	if deliver, err := store.ObserveCodexTopicReadState(ctx, project.TelegramChatID, 7, false, 88); err != nil || deliver {
		t.Fatalf("unchanged read state deliver=%v err=%v", deliver, err)
	}
	if deliver, err := store.ObserveCodexTopicReadState(ctx, project.TelegramChatID, 7, true, 88); err != nil || deliver {
		t.Fatalf("unread state deliver=%v err=%v", deliver, err)
	}
	if deliver, err := store.ObserveCodexTopicReadState(ctx, project.TelegramChatID, 7, false, 91); err != nil || !deliver {
		t.Fatalf("new read state deliver=%v err=%v", deliver, err)
	}

	if err := store.ReserveCodexOutboundMessage(ctx, project.TelegramChatID, 7, "**hello**"); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCodexOutboundMessage(ctx, project.TelegramChatID, 7, 88, "**hello**"); err != nil {
		t.Fatal(err)
	}
	consumed, err := store.ConsumeCodexOutboundMessage(ctx, project.TelegramChatID, 7, 88, "hello")
	if err != nil || !consumed {
		t.Fatalf("consume=%v err=%v", consumed, err)
	}
	if consumed, err := store.ConsumeCodexOutboundMessage(ctx, project.TelegramChatID, 7, 88, "hello"); err != nil || consumed {
		t.Fatalf("second consume=%v err=%v", consumed, err)
	}
}
