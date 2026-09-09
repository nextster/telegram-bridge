package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mediaStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMediaBudgetReservationIsAtomicAndPersistent(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, id := range []string{"a", "b"} {
		if _, err := s.EnqueueMedia(ctx, MediaJob{ID: id, AccountID: 1, Chat: "chat:1", MessageID: 1, CreatedAt: now.Unix(), Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimMedia(ctx, 1, now); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Go(func() { results <- s.ReserveMedia(ctx, id, "{}", 60, 100, 200, now) })
	}
	wg.Wait()
	close(results)
	ok, blocked := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrMediaBudget) {
			blocked++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || blocked != 1 {
		t.Fatal("parallel reservations exceeded daily budget")
	}
	var spent int64
	if err := s.db.QueryRow(`SELECT SUM(amount) FROM media_charges`).Scan(&spent); err != nil || spent != 60 {
		t.Fatalf("spent=%d err=%v", spent, err)
	}
	var remaining string
	s.db.QueryRow(`SELECT id FROM media_jobs WHERE status='preparing'`).Scan(&remaining)
	if err := s.ReserveMedia(ctx, remaining, "{}", 60, 100, 100, now.Add(24*time.Hour)); !errors.Is(err, ErrMediaBudget) {
		t.Fatal("total budget reset with day")
	}
	if err := s.RecordMediaCost(ctx, map[string]string{"a": "b", "b": "a"}[remaining], 120); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveMedia(ctx, remaining, "{}", 60, 100, 150, now.Add(24*time.Hour)); !errors.Is(err, ErrMediaBudget) {
		t.Fatal("reported cost increase ignored")
	}
}

func TestMediaQueueCapacityStillReturnsDuplicates(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	for n := range 100 {
		if _, err := s.EnqueueMedia(ctx, MediaJob{ID: fmt.Sprint(n), AccountID: 1, Chat: "chat:1", MessageID: n + 1, Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnqueueMedia(ctx, MediaJob{ID: "overflow", AccountID: 1, Payload: "{}"}); !errors.Is(err, ErrMediaLimit) {
		t.Fatal("queue cap not enforced")
	}
	if _, err := s.EnqueueMedia(ctx, MediaJob{ID: "0", AccountID: 1}); err != nil {
		t.Fatal("full queue rejected cached job")
	}
	if _, err := s.ClaimMedia(ctx, 2, time.Now()); err == nil {
		t.Fatal("job claimed by wrong account")
	}
}

func TestMediaRecoveryPreservesCompletedAndStopsExhausted(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	for _, id := range []string{"complete", "exhausted"} {
		if _, err := s.EnqueueMedia(ctx, MediaJob{ID: id, AccountID: 1, Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE media_jobs SET status='completed',transcript='saved' WHERE id='complete'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE media_jobs SET status='preparing',attempts=3 WHERE id='exhausted'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverMedia(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	done, _ := s.MediaJob(ctx, "complete")
	failed, _ := s.MediaJob(ctx, "exhausted")
	if done.Status != "completed" || done.Transcript != "saved" || failed.Status != "failed" || failed.ErrorCode != "retry_exhausted" {
		t.Fatal("incorrect recovery")
	}
}
