package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

type historyReader struct {
	page  monitor.HistoryPage
	calls atomic.Int32
	mu    sync.Mutex
	opts  monitor.HistoryOptions
}

func (r *historyReader) ListDialogs(context.Context, string, int) ([]monitor.TelegramDialog, error) {
	return nil, nil
}
func (r *historyReader) SearchMessages(context.Context, monitor.MessageSearchOptions) ([]monitor.TelegramMessage, error) {
	return nil, nil
}
func (r *historyReader) GetHistoryPage(_ context.Context, opts monitor.HistoryOptions) (monitor.HistoryPage, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.opts = opts
	r.mu.Unlock()
	return r.page, nil
}

type historySource struct{ mcpMediaSource }

func (s historySource) Attachment(ctx context.Context, chat string, id int) (media.Attachment, error) {
	a, err := s.mcpMediaSource.Attachment(ctx, chat, id)
	if id == 11 {
		a.Kind = "video_note"
	}
	if id == 12 {
		a.Kind = "image"
	}
	return a, err
}

func historyFixture(t *testing.T) (*Server, *db.Store, *historyReader) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := config.DefaultMediaConfig(path)
	cfg.Enabled, cfg.APIKey = true, "never-used"
	service, err := media.New(cfg, store, historySource{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	r := &historyReader{page: monitor.HistoryPage{HasMore: true, NextOffsetID: 10}}
	for i, kind := range []string{"voice", "video_note", "image", "document", "text"} {
		r.page.Messages = append(r.page.Messages, monitor.TelegramMessage{ID: 10 + i, Chat: monitor.TelegramPeer{Key: "chat:1"}, MediaKind: kind, Date: time.Unix(1700000000, 0), ReplyToID: 9})
	}
	return New(r, "secret", Options{Media: service}), store, r
}

func TestHistoryPaidBoundaryAndRangeValidation(t *testing.T) {
	s, store, reader := historyFixture(t)
	ctx := context.Background()
	_, out, err := s.getHistory(ctx, nil, getHistoryInput{Chat: "chat:1", MinDate: "2023-01-01"})
	if err != nil || out.Media != nil || len(out.Messages) != 5 || !out.HasMore {
		t.Fatal("free history changed")
	}
	if _, err := store.ClaimMedia(ctx, 1, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("ordinary history queued paid work")
	}
	for _, in := range []getHistoryInput{
		{Chat: "chat:1", ProcessMedia: true, MinDate: "2023-01-01"},
		{Chat: "chat:1", ProcessMedia: true, ConfirmPaid: true},
		{Chat: "chat:1", MinDate: "2024-01-01", MaxDate: "2023-01-01"},
		{Chat: "chat:1", MinDate: "bad"},
		{Chat: "chat:1", Limit: 101},
		{Chat: "chat:1", OffsetID: -1},
		{Chat: "/etc/passwd"},
	} {
		if _, _, err := s.getHistory(ctx, nil, in); err == nil {
			t.Fatal("invalid history accepted")
		}
	}
	if reader.calls.Load() != 1 {
		t.Fatal("invalid history made an RPC")
	}
	zero := 0
	in := getHistoryInput{Chat: "chat:1", MinDate: "2023-01-01", ProcessMedia: true, ConfirmPaid: true, WaitSeconds: &zero}
	_, out, err = s.getHistory(ctx, nil, in)
	if err != nil || out.Media == nil || len(out.Media.Items) != 3 || out.Media.Settled || out.MaxDate == "" {
		t.Fatal("paid page did not queue exactly supported media")
	}
	if out.Messages[0].ReplyToID != 9 || out.NextOffsetID != 10 || !out.HasMore {
		t.Fatal("source/pagination lost")
	}
	if len(out.SkippedMedia) != 1 || out.SkippedMedia[0].MessageID != 13 || out.SkippedMedia[0].Kind != "document" || out.SkippedMedia[0].Reason != "unsupported_attachment" {
		t.Fatal("unsupported attachment silently skipped")
	}
	reader.mu.Lock()
	opts := reader.opts
	reader.mu.Unlock()
	if opts.MinDate.IsZero() || opts.MaxDate.IsZero() || opts.Chat != in.Chat {
		t.Fatal("date bounds not applied")
	}
	in.MaxDate = out.MaxDate
	_, duplicate, err := s.getHistory(ctx, nil, in)
	if err != nil {
		t.Fatal(err)
	}
	for i := range out.Media.Items {
		if out.Media.Items[i].Job.ID != duplicate.Media.Items[i].Job.ID {
			t.Fatal("repeated history created new job")
		}
	}
}

func completeHistoryJobs(ctx context.Context, store *db.Store, count int) error {
	for completed := 0; completed < count; {
		if err := ctx.Err(); err != nil {
			return err
		}
		j, err := store.ClaimMedia(ctx, 1, time.Now())
		if errors.Is(err, sql.ErrNoRows) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		if err := store.ReserveMedia(ctx, j.ID, j.Payload, 100, 1000000, 5000000, time.Now()); err != nil {
			return err
		}
		j.Status = "completed"
		if j.MessageID == 12 {
			j.ImageDescription, j.ImageText = "scene", "visible text"
		} else {
			j.Transcript = "spoken words"
		}
		meta, _ := json.Marshal(media.Result{Languages: []string{"en"}, LanguageSource: "provider"})
		j.ResultMeta = string(meta)
		if err := store.FinishMedia(ctx, j); err != nil {
			return err
		}
		completed++
	}
	return nil
}

func TestConcurrentHistoryWaitsReuseBatchAndSingleResults(t *testing.T) {
	s, store, _ := historyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	refs := []media.Reference{{Chat: "chat:1", MessageID: 10}, {Chat: "chat:1", MessageID: 12}}
	batch, err := s.media.StartBatch(ctx, refs, media.BatchOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	single, err := s.media.Start(ctx, "chat:1", 11, "transcription", media.Options{}, true)
	if err != nil {
		t.Fatal(err)
	}
	worker := make(chan error, 1)
	go func() { worker <- completeHistoryJobs(ctx, store, 3) }()
	var group sync.WaitGroup
	wait := 2
	for range 8 {
		group.Go(func() {
			_, out, err := s.getHistory(ctx, nil, getHistoryInput{Chat: "chat:1", MinDate: "2023-01-01", MaxDate: "2024-01-01", ProcessMedia: true, ConfirmPaid: true, WaitSeconds: &wait})
			if err != nil {
				t.Error(err)
				return
			}
			if !out.Media.Settled || !out.Media.AllSucceeded || out.Media.TimedOut {
				t.Error("history returned before completion")
				return
			}
			if out.Media.Items[0].Job.ID != batch.Items[0].Job.ID || out.Media.Items[1].Job.ID != single.ID || out.Media.Items[2].Job.ID != batch.Items[1].Job.ID {
				t.Error("history did not share jobs")
			}
			for _, item := range out.Media.Items {
				if item.Job.ProviderAttempts != 1 {
					t.Error("duplicated paid submission")
				}
			}
			image := out.Media.Items[2].Job.Result
			if image.Description != "scene" || image.OCRText != "visible text" || image.Text != "" {
				t.Error("image fields mixed")
			}
		})
	}
	group.Wait()
	if err := <-worker; err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimMedia(ctx, 1, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unprocessed duplicate left")
	}
}

func TestHistoryWaitTimeoutLeavesDurableJobs(t *testing.T) {
	s, store, _ := historyFixture(t)
	wait := 1
	_, out, err := s.getHistory(context.Background(), nil, getHistoryInput{Chat: "chat:1", MinDate: "2023-01-01", ProcessMedia: true, ConfirmPaid: true, WaitSeconds: &wait})
	if err != nil || out.Media == nil || !out.Media.TimedOut || out.Media.Settled || out.Media.AllSucceeded {
		t.Fatal("timeout falsely reported completion")
	}
	for _, item := range out.Media.Items {
		j, err := store.MediaJob(context.Background(), item.Job.ID)
		if err != nil || j.Status != "queued" || j.ProviderAttempts != 0 {
			t.Fatal("waiter timeout changed durable job")
		}
	}
}

func TestHistoryReportsUnsupportedVideoButNotLinkPreview(t *testing.T) {
	s, _, reader := historyFixture(t)
	reader.page.Messages = nil
	for i, kind := range []string{"video", "unsupported", "web_page", "text"} {
		reader.page.Messages = append(reader.page.Messages, monitor.TelegramMessage{ID: i + 1, Chat: monitor.TelegramPeer{Key: "chat:1"}, MediaKind: kind})
	}
	zero := 0
	_, out, err := s.getHistory(context.Background(), nil, getHistoryInput{Chat: "chat:1", MinDate: "2023-01-01", ProcessMedia: true, ConfirmPaid: true, WaitSeconds: &zero})
	if err != nil || out.Media == nil || len(out.Media.Items) != 0 || len(out.SkippedMedia) != 2 {
		t.Fatal("unsupported scope hidden or queued", err)
	}
	if out.SkippedMedia[0].Kind != "video" || out.SkippedMedia[1].Kind != "unsupported" {
		t.Fatal("link previews treated as media files")
	}
}
