package db

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
	"time"
)

func budgetJob(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	if _, err := s.EnqueueMedia(context.Background(), MediaJob{ID: id, AccountID: 1, Chat: "chat:1", MessageID: 1, CreatedAt: now.Unix(), Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimMedia(context.Background(), []int64{1}, now); err != nil {
		t.Fatal(err)
	}
}

func TestSeptemberMediaLedgerRegression(t *testing.T) {
	data, err := os.ReadFile("testdata/media-budget-20260909.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		ID, Status       string
		ErrorCode        string `json:"error_code"`
		Attempts         int
		ProviderAttempts int      `json:"provider_attempts"`
		Cost             *float64 `json:"cost_usd"`
		Reservations     []struct {
			Day    string
			Amount int64
		}
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	s := mediaStore(t)
	ctx := context.Background()
	for _, f := range fixtures {
		meta, _ := json.Marshal(struct {
			Cost *float64 `json:"cost_usd,omitempty"`
		}{f.Cost})
		if _, err = s.db.Exec(`INSERT INTO media_jobs(id,account_id,chat,message_id,status,attempts,provider_attempts,created_at,updated_at,error_code,payload,result_meta) VALUES(?,1,'chat:1',1,?,?,?,0,0,?,'{}',?)`, f.ID, f.Status, f.Attempts, f.ProviderAttempts, f.ErrorCode, string(meta)); err != nil {
			t.Fatal(err)
		}
		for _, r := range f.Reservations {
			if _, err = s.db.Exec(`INSERT INTO media_charges(job_id,day,amount) VALUES(?,?,?)`, f.ID, r.Day, r.Amount); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Date(2026, 9, 9, 17, 26, 34, 0, time.UTC)
	budgetJob(t, s, "new-image", now)
	var b *MediaBudget
	if err = s.ReserveMedia(ctx, "new-image", "{}", 20000, 1000000, 5000000, now); !errors.As(err, &b) || b.DailyUsed != 999300 {
		t.Fatalf("did not reproduce original refusal: %+v %v", b, err)
	}
	if err = s.ReconcileMediaCosts(ctx); err != nil {
		t.Fatal(err)
	}
	var effective int64
	s.db.QueryRow(`SELECT SUM(COALESCE(s.amount,c.amount)) FROM media_charges c LEFT JOIN media_charge_settlements s ON s.charge_id=c.id`).Scan(&effective)
	if effective != 132898 {
		t.Fatalf("expected 90098 known + 40000 uncertain + 2800 earlier attempt, got %d", effective)
	}
	if err = s.ReserveMedia(ctx, "new-image", "{}", 20000, 1000000, 5000000, now); err != nil {
		t.Fatal("corrected ledger still refuses affordable work", err)
	}
	var failed int
	s.db.QueryRow(`SELECT COUNT(*) FROM media_jobs WHERE status='failed' AND error_code='budget_exceeded' AND provider_attempts=0`).Scan(&failed)
	if failed != 12 {
		t.Fatal("reconciliation automatically restarted historical jobs")
	}
}

func TestMediaSettlementReleasesOnlyKnownCostAndWakesBudgetWait(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 23, 59, 0, 0, time.UTC)
	budgetJob(t, s, "a", now)
	if err := s.ReserveMedia(ctx, "a", "{}", 80, 100, 200, now); err != nil {
		t.Fatal(err)
	}
	budgetJob(t, s, "b", now)
	var budget *MediaBudget
	if err := s.ReserveMedia(ctx, "b", "{}", 80, 100, 200, now); !errors.As(err, &budget) {
		t.Fatalf("missing budget snapshot: %v", err)
	}
	if budget.Scope != "daily" || budget.DailyUsed != 80 || budget.TotalUsed != 80 {
		t.Fatalf("wrong snapshot: %+v", budget)
	}
	if err := s.DeferMediaBudget(ctx, "b", budget, now); err != nil {
		t.Fatal(err)
	}
	wait, _ := s.MediaJob(ctx, "b")
	if wait.Status != "retry_wait" || wait.Attempts != 0 || wait.ProviderAttempts != 0 || wait.RetryAt != now.Add(time.Minute).Unix() {
		t.Fatalf("bad wait: %+v", wait)
	}
	if _, err := s.ClaimMedia(ctx, []int64{1}, now); err == nil {
		t.Fatal("claimed before budget window")
	}
	if err := s.RecoverMedia(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMediaCost(ctx, "a", 20); err != nil {
		t.Fatal(err)
	}
	wait, _ = s.MediaJob(ctx, "b")
	if wait.RetryAt != 0 {
		t.Fatal("settlement did not wake waiter")
	}
	if _, err := s.ClaimMedia(ctx, []int64{1}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveMedia(ctx, "b", "{}", 80, 100, 200, now); err != nil {
		t.Fatal("actual cost not used", err)
	}
	var reserved, settled int64
	s.db.QueryRow(`SELECT SUM(amount) FROM media_charges`).Scan(&reserved)
	s.db.QueryRow(`SELECT amount FROM media_charge_settlements`).Scan(&settled)
	if reserved != 160 || settled != 20 {
		t.Fatal("original reservation lost")
	}
	if err := s.RecordMediaCost(ctx, "b", 0); err != nil {
		t.Fatal("zero cost rejected", err)
	}
	if err := s.RecordMediaCost(ctx, "a", 5); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT SUM(amount) FROM media_charge_settlements`).Scan(&settled)
	if settled != 20 {
		t.Fatal("duplicate settlement lowered an already verified charge")
	}
}

func TestMediaBudgetDayAndTotalScopes(t *testing.T) {
	for _, scope := range []string{"daily", "total", "request"} {
		t.Run(scope, func(t *testing.T) {
			s := mediaStore(t)
			ctx := context.Background()
			now := time.Date(2026, 9, 9, 23, 59, 0, 0, time.FixedZone("local", 4*3600))
			budgetJob(t, s, "a", now)
			if err := s.ReserveMedia(ctx, "a", "{}", 80, 100, 100, now); err != nil {
				t.Fatal(err)
			}
			budgetJob(t, s, "b", now)
			daily, total, amount := int64(100), int64(200), int64(80)
			if scope == "total" {
				daily, total = 200, 100
			}
			if scope == "request" {
				amount = 101
			}
			var b *MediaBudget
			if err := s.ReserveMedia(ctx, "b", "{}", amount, daily, total, now); !errors.As(err, &b) || b.Scope != scope {
				t.Fatalf("bad scope: %+v %v", b, err)
			}
			if err := s.DeferMediaBudget(ctx, "b", b, now); err != nil {
				t.Fatal(err)
			}
			wait, _ := s.MediaJob(ctx, "b")
			if scope == "daily" {
				want := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
				if wait.RetryAt != want.Unix() {
					t.Fatal("window not UTC")
				}
				if _, err := s.ClaimMedia(ctx, []int64{1}, want); err != nil {
					t.Fatal(err)
				}
				if err := s.ReserveMedia(ctx, "b", "{}", amount, daily, total, want); err != nil {
					t.Fatal("previous day counted toward daily cap", err)
				}
			} else if wait.RetryAt != math.MaxInt64 {
				t.Fatal("non-daily budget automatically resets")
			}
		})
	}
}

func TestMediaReconciliationOnlyLastCompletedAttempt(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	for _, id := range []string{"complete", "failed", "uncertain", "unknown", "invalid", "zero", "blocked"} {
		if _, err := s.EnqueueMedia(ctx, MediaJob{ID: id, AccountID: 1, Chat: "chat:1", MessageID: 1, Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO media_charges(job_id,day,amount) VALUES(?,'2026-09-08',20000)`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`UPDATE media_jobs SET status='completed',result_meta='{"cost_usd":0.001}' WHERE id='complete'`,
		`INSERT INTO media_charges(job_id,day,amount) VALUES('complete','2026-09-09',20000)`,
		`UPDATE media_jobs SET status='failed',result_meta='{"cost_usd":0.001}' WHERE id='failed'`,
		`UPDATE media_jobs SET status='uncertain' WHERE id='uncertain'`,
		`UPDATE media_jobs SET status='completed',result_meta='{}' WHERE id='unknown'`,
		`UPDATE media_jobs SET status='completed',result_meta='{"cost_usd":-1}' WHERE id='invalid'`,
		`UPDATE media_jobs SET status='completed',result_meta='{"cost_usd":0}' WHERE id='zero'`,
		`UPDATE media_jobs SET status='failed',error_code='budget_exceeded' WHERE id='blocked'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.ReconcileMediaCosts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var n, total int64
	s.db.QueryRow(`SELECT COUNT(*),SUM(amount) FROM media_charge_settlements`).Scan(&n, &total)
	if n != 2 || total != 1000 {
		t.Fatalf("incorrect reconciliation: %d %d", n, total)
	}
	var effective int64
	s.db.QueryRow(`SELECT SUM(COALESCE(s.amount,c.amount)) FROM media_charges c LEFT JOIN media_charge_settlements s ON s.charge_id=c.id`).Scan(&effective)
	if effective != 121000 {
		t.Fatalf("unverified attempts released: %d", effective)
	}
	j, _ := s.MediaJob(ctx, "blocked")
	if j.Status != "failed" {
		t.Fatal("historical budget failure requeued")
	}
}

func TestMediaSettlementBetweenRefusalAndDeferralNotLost(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	now := time.Now()
	budgetJob(t, s, "a", now)
	if err := s.ReserveMedia(ctx, "a", "{}", 80, 100, 200, now); err != nil {
		t.Fatal(err)
	}
	budgetJob(t, s, "b", now)
	var b *MediaBudget
	if err := s.ReserveMedia(ctx, "b", "{}", 80, 100, 200, now); !errors.As(err, &b) {
		t.Fatal(err)
	}
	if err := s.RecordMediaCost(ctx, "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferMediaBudget(ctx, "b", b, now); err != nil {
		t.Fatal(err)
	}
	if j, err := s.ClaimMedia(ctx, []int64{1}, now); err != nil || j.ID != "b" {
		t.Fatal("lost settlement wakeup", err)
	}
}

func TestMediaResultAndCostCommitAtomically(t *testing.T) {
	s := mediaStore(t)
	ctx := context.Background()
	now := time.Now()
	budgetJob(t, s, "job", now)
	j, _ := s.MediaJob(ctx, "job")
	j.Status = "completed"
	j.Transcript = "fixture"
	j.ResultMeta = `{"cost_usd":0.000001}`
	cost := int64(1)
	if err := s.FinishMediaWithCost(ctx, j, &cost); err == nil {
		t.Fatal("completed cost without a submission")
	}
	unchanged, _ := s.MediaJob(ctx, "job")
	if unchanged.Status != "preparing" || unchanged.Transcript != "" {
		t.Fatal("failed settlement partially committed result")
	}
	if err := s.ReserveMedia(ctx, "job", "{}", 80, 100, 200, now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMediaWithCost(ctx, j, &cost); err != nil {
		t.Fatal(err)
	}
	done, _ := s.MediaJob(ctx, "job")
	var spent int64
	s.db.QueryRow(`SELECT amount FROM media_charge_settlements`).Scan(&spent)
	if done.Status != "completed" || done.Transcript != "fixture" || spent != 1 {
		t.Fatal("result and cost not persisted together")
	}
}
