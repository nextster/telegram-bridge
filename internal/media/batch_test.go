package media

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type batchSource struct{ downloads atomic.Int32 }

type interruptedProcessor struct{ cancel context.CancelFunc }

func (p interruptedProcessor) Prepare(context.Context, string, string, Attachment, int) (Prepared, error) {
	p.cancel()
	return Prepared{}, Fail("invalid_media")
}

func TestInterruptedPreparationRemainsRetryable(t *testing.T) {
	s, _, provider := testService(t)
	j := startTestJob(t, s, "transcription", Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.processor = interruptedProcessor{cancel: cancel}
	if worked, err := s.runOne(ctx); !worked || err != nil {
		t.Fatalf("interrupted preparation: %v", err)
	}
	got := getTestJob(t, s, j.ID)
	if got.Status != "retry_wait" || got.ErrorCode != "interrupted_before_submission" || provider.calls != 0 {
		t.Fatal("shutdown turned unpaid preparation into a terminal failure")
	}
}

func (s *batchSource) LiveAccounts() []int64 { return []int64{testAccount} }
func (s *batchSource) Attachment(_ context.Context, accountID int64, chat string, id int) (Attachment, error) {
	if accountID != testAccount {
		return Attachment{}, Fail("telegram_account_unavailable")
	}
	kind := "voice"
	if id%3 == 0 {
		kind = "image"
	}
	if id == 99 {
		kind = "document"
	}
	return Attachment{AccountID: 1, Chat: chat, MessageID: id, Kind: kind, Supported: id != 99, Size: 5, Duration: 10, Fingerprint: Digest(id)}, nil
}
func (s *batchSource) Download(_ context.Context, _ int64, _ Attachment, w io.Writer) error {
	s.downloads.Add(1)
	_, err := io.WriteString(w, "audio")
	return err
}

type concurrentProvider struct {
	active, peak, calls atomic.Int32
	entered             chan struct{}
	release             chan struct{}
}

func (p *concurrentProvider) Recognize(ctx context.Context, operation string, _ Options, _ Prepared) (Result, error) {
	p.calls.Add(1)
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for old := p.peak.Load(); active > old && !p.peak.CompareAndSwap(old, active); old = p.peak.Load() {
	}
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.release != nil {
		select {
		case <-ctx.Done():
			return Result{}, &Fault{Code: "provider_outcome_unknown", Uncertain: true}
		case <-p.release:
		}
	}
	if operation == "image" {
		return Result{Description: "description", OCRText: "OCR"}, nil
	}
	return Result{Text: "speech"}, nil
}

func runPool(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker pool: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker pool did not join on shutdown")
		}
	})
}

func TestParallelBatchAndSingleCallsShareJobsAndWaiters(t *testing.T) {
	s, _, _ := testService(t)
	s.now = time.Now
	source := &batchSource{}
	s.source = source
	p := &concurrentProvider{entered: make(chan struct{}, 20), release: make(chan struct{})}
	s.provider = p
	refs := []Reference{}
	for id := 1; id <= 9; id++ {
		refs = append(refs, Reference{Chat: "channel:42", MessageID: id})
	}
	refs = append(refs, refs[0])
	settings := BatchOptions{Audio: Options{Keywords: []string{" Realize ", "timeline"}}}
	var group sync.WaitGroup
	results := make(chan Batch, 10)
	errors := make(chan error, 10)
	for range 10 {
		group.Go(func() {
			batch, err := s.StartBatch(context.Background(), testAccount, refs, settings, true)
			results <- batch
			errors <- err
		})
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Batch
	for batch := range results {
		if first.Items == nil {
			first = batch
		}
		for i, item := range batch.Items {
			if item.Job == nil || item.Job.ID != first.Items[i].Job.ID {
				t.Fatal("duplicate batch job")
			}
		}
	}
	single, err := s.Start(context.Background(), testAccount, refs[0].Chat, refs[0].MessageID, "transcription", settings.Audio, true)
	if err != nil || single.ID != first.Items[0].Job.ID {
		t.Fatal("single and batch do not share cache")
	}
	if settings.Audio.Keywords[0] != " Realize " {
		t.Fatal("shared input mutated")
	}
	runPool(t, s)
	for range 3 {
		select {
		case <-p.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("three provider calls did not run concurrently")
		}
	}
	if p.active.Load() != 3 || p.calls.Load() != 3 {
		t.Fatal("concurrency limit broken")
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("second pool allowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	interrupted, err := s.WaitBatch(ctx, testAccount, first, time.Second)
	if err != nil || !interrupted.TimedOut || interrupted.Settled {
		t.Fatal("cancelled waiter lost pending state")
	}
	close(p.release)
	finished, err := s.WaitBatch(context.Background(), testAccount, first, 10*time.Second)
	if err != nil || !finished.Settled || !finished.AllSucceeded || finished.TimedOut {
		t.Fatalf("batch not completed: %+v %v", finished, err)
	}
	if p.calls.Load() != 9 || source.downloads.Load() != 9 || p.peak.Load() != 3 {
		t.Fatal("duplicate calls or incorrect concurrency")
	}
	for _, item := range finished.Items {
		if item.Job.ProviderAttempts != 1 {
			t.Fatal("duplicate paid attempt")
		}
		if item.Job.Operation == "image" && (item.Job.Result.Description != "description" || item.Job.Result.OCRText != "OCR" || item.Job.Result.Text != "") {
			t.Fatal("mixed image fields")
		}
	}
	repeated, err := s.StartBatch(context.Background(), testAccount, refs, settings, true)
	if err != nil || !repeated.AllSucceeded || p.calls.Load() != 9 {
		t.Fatal("cached batch reprocessed")
	}
}

func TestBatchValidationTimeoutAndPartialErrors(t *testing.T) {
	s, _, _ := testService(t)
	s.source = &batchSource{}
	for _, refs := range [][]Reference{nil, make([]Reference, 101), {{Chat: "/etc/passwd", MessageID: 1}}} {
		if _, err := s.StartBatch(context.Background(), testAccount, refs, BatchOptions{}, true); err == nil {
			t.Fatal("invalid batch allowed")
		}
	}
	refs := []Reference{{Chat: "chat:1", MessageID: 1}, {Chat: "chat:1", MessageID: 99}}
	if _, err := s.StartBatch(context.Background(), testAccount, refs, BatchOptions{}, false); err == nil {
		t.Fatal("implicit paid batch")
	}
	if _, err := s.StartBatch(context.Background(), testAccount, refs, BatchOptions{Audio: Options{Model: "bad"}}, true); err == nil {
		t.Fatal("invalid model")
	}
	batch, err := s.StartBatch(context.Background(), testAccount, refs, BatchOptions{}, true)
	if err != nil || batch.Items[1].ErrorCode != "unsupported_attachment" || batch.Items[0].Job == nil {
		t.Fatal("wrong per-item errors")
	}
	timed, err := s.WaitBatch(context.Background(), testAccount, batch, time.Millisecond)
	if err != nil || !timed.TimedOut || timed.Settled || timed.AllSucceeded {
		t.Fatal("pending batch called complete")
	}
	if _, err := s.WaitBatch(context.Background(), testAccount, batch, MaxWait+time.Second); err == nil {
		t.Fatal("unbounded wait")
	}
	ref := JobReference{Chat: refs[0].Chat, MessageID: 1, JobID: batch.Items[0].Job.ID}
	if got, err := s.GetBatch(context.Background(), testAccount, []JobReference{ref}); err != nil || got.Items[0].Job.ID != ref.JobID {
		t.Fatal("batch polling failed")
	}
	ref.Chat = "chat:2"
	if _, err := s.GetBatch(context.Background(), testAccount, []JobReference{ref}); err == nil {
		t.Fatal("cross-chat batch polling")
	}
}

func TestParallelWorkersShareBudget(t *testing.T) {
	s, _, _ := testService(t)
	s.now = time.Now
	s.source = &batchSource{}
	s.cfg.DailyBudgetMicros = 2000
	p := &concurrentProvider{}
	s.provider = p
	refs := []Reference{{Chat: "chat:1", MessageID: 1}, {Chat: "chat:1", MessageID: 2}, {Chat: "chat:1", MessageID: 4}, {Chat: "chat:1", MessageID: 5}}
	batch, err := s.StartBatch(context.Background(), testAccount, refs, BatchOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	runPool(t, s)
	batch, err = s.WaitBatch(context.Background(), testAccount, batch, 5*time.Second)
	if err != nil || batch.Settled || !batch.TimedOut || batch.AllSucceeded || p.calls.Load() != 2 {
		t.Fatalf("shared budget failed: calls=%d err=%v", p.calls.Load(), err)
	}
	failed := 0
	for _, item := range batch.Items {
		if item.Job.ErrorCode == "budget_exceeded" {
			if item.Job.Status != "budget_wait" || item.Job.Attempts != 0 || item.Job.ProviderAttempts != 0 || item.Job.NextAttemptAt == nil {
				t.Fatal("budget wait consumed attempt or hid next window")
			}
			failed++
		}
	}
	if failed != 2 {
		t.Fatal("wrong budget failures")
	}
}

func TestRateLimitPausesOtherJobsAndSurvivesRecovery(t *testing.T) {
	s, _, p := testService(t)
	s.source = &batchSource{}
	batch, err := s.StartBatch(context.Background(), testAccount, []Reference{{Chat: "chat:1", MessageID: 1}, {Chat: "chat:1", MessageID: 2}}, BatchOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	p.err = Retry("openrouter_rate_limited", 30*time.Second)
	runTestJob(t, s)
	if err := s.store.RecoverMedia(context.Background(), s.now()); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.runOne(context.Background()); err != nil || worked || p.calls != 1 {
		t.Fatal("other job ignored shared cooldown")
	}
	until, err := s.store.MediaCooldown(context.Background())
	if err != nil || until <= s.now().Unix() {
		t.Fatal("cooldown not persisted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.waitForProvider(ctx); err == nil || fault(err).RetryAfter == 0 {
		t.Fatal("cancelled provider wait ignored")
	}
	next := s.now().Add(time.Minute)
	s.now = func() time.Time { return next }
	p.err = nil
	runTestJob(t, s)
	runTestJob(t, s)
	batch, err = s.WaitBatch(context.Background(), testAccount, batch, time.Second)
	if err != nil || !batch.AllSucceeded || p.calls != 3 {
		t.Fatal("cooldown did not resume shared jobs")
	}
}
