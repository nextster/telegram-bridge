package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
)

type fakeSender struct {
	calls       atomic.Int32
	fail        bool
	unavailable bool
}

func (f *fakeSender) CheckNotificationChat(context.Context, int64) error {
	if f.unavailable {
		return errors.New("private token")
	}
	return nil
}
func (f *fakeSender) SendNotification(context.Context, int64, string, string) (int, error) {
	f.calls.Add(1)
	if f.fail {
		return 0, errors.New("private token")
	}
	return 42, nil
}

const (
	testAccount  = int64(111)
	otherAccount = int64(222)
	testGroup    = int64(-1001234567890)
)

// fakeSenders maps each account to its own sender.
type fakeSenders map[int64]*fakeSender

func (f fakeSenders) NotificationSender(_ context.Context, accountID int64) (NotificationSender, error) {
	sender, ok := f[accountID]
	if !ok {
		return nil, errors.New("not connected")
	}
	return sender, nil
}

func setupNotifications(t *testing.T) (*Notifications, *fakeSender, string) {
	t.Helper()
	path := t.TempDir() + "/notifications.db"
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.AddNotificationChat(context.Background(), testAccount, testGroup, "Releases", time.Now()); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	return NewNotifications(store, fakeSenders{testAccount: sender}), sender, path
}
func releaseInput() NotificationInput {
	return NotificationInput{Chat: "channel:1234567890", EventID: "example:ios:0.1.0:16", Text: "Example 0.1.0 (16)"}
}

func TestNotificationIdempotencySurvivesReopen(t *testing.T) {
	n, sender, path := setupNotifications(t)
	ctx := context.Background()
	first, err := n.Send(ctx, testAccount, releaseInput())
	if err != nil || first.Status != "sent" || first.MessageID != 42 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	n.store.Close()
	store, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	n = NewNotifications(store, fakeSenders{testAccount: sender})
	again, err := n.Send(ctx, testAccount, releaseInput())
	if err != nil || again != first || sender.calls.Load() != 1 {
		t.Fatalf("again=%+v err=%v calls=%d", again, err, sender.calls.Load())
	}
	input := releaseInput()
	input.Text = "changed"
	if _, err := n.Send(ctx, testAccount, input); err == nil {
		t.Fatal("accepted conflicting text")
	}
	input.EventID += ":new"
	if _, err := n.Send(ctx, testAccount, input); err != nil {
		t.Fatal(err)
	}
	if sender.calls.Load() != 2 {
		t.Fatal("new event was not sent")
	}
}

func TestNotificationFailureIsRedactedAndNotRetried(t *testing.T) {
	n, sender, _ := setupNotifications(t)
	sender.fail = true
	for range 2 {
		_, err := n.Send(context.Background(), testAccount, releaseInput())
		if err == nil || strings.Contains(err.Error(), "private token") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if sender.calls.Load() != 1 {
		t.Fatal("uncertain delivery was retried")
	}
}

func TestNotificationPreflightFailureRemainsRetryable(t *testing.T) {
	n, sender, _ := setupNotifications(t)
	sender.unavailable = true
	if _, err := n.Send(context.Background(), testAccount, releaseInput()); err == nil || strings.Contains(err.Error(), "private token") {
		t.Fatalf("unexpected error: %v", err)
	}
	sender.unavailable = false
	if _, err := n.Send(context.Background(), testAccount, releaseInput()); err != nil {
		t.Fatal(err)
	}
	if sender.calls.Load() != 1 {
		t.Fatal("wrong send count")
	}
}

func TestNotificationRejectsInvalidOrUnauthorizedInput(t *testing.T) {
	n, sender, _ := setupNotifications(t)
	inputs := []NotificationInput{}
	for _, chat := range []string{"user:1234567890", "channel:1", "chat:1234567890", "channel:01234567890", "channel:9999999999999999999", "channel:+1234567890"} {
		input := releaseInput()
		input.Chat = chat
		inputs = append(inputs, input)
	}
	for _, event := range []string{"", strings.Repeat("x", 129), "hello\n", "a/b"} {
		input := releaseInput()
		input.EventID = event
		inputs = append(inputs, input)
	}
	for _, text := range []string{"", " \n", strings.Repeat("a", 4097), strings.Repeat("🙂", 2049)} {
		input := releaseInput()
		input.Text = text
		inputs = append(inputs, input)
	}
	for _, input := range inputs {
		if _, err := n.Send(context.Background(), testAccount, input); err == nil {
			t.Errorf("accepted invalid input %+v", input)
		}
	}
	if sender.calls.Load() != 0 {
		t.Fatal("invalid input sent a message")
	}
	input := releaseInput()
	input.Text = strings.Repeat("🙂", 2048)
	if _, err := n.Send(context.Background(), testAccount, input); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationConcurrentRequestsSendOnce(t *testing.T) {
	n, sender, _ := setupNotifications(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _, _ = n.Send(context.Background(), testAccount, releaseInput()) })
	}
	wg.Wait()
	if sender.calls.Load() != 1 {
		t.Fatalf("sent %d messages", sender.calls.Load())
	}
}

func TestNotificationEmptyAllowlistIsDisabled(t *testing.T) {
	n, sender, _ := setupNotifications(t)
	if err := n.store.RemoveNotificationChat(context.Background(), testAccount, testGroup); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(context.Background(), testAccount, releaseInput()); err == nil {
		t.Fatal("empty allowlist accepted notification")
	}
	if sender.calls.Load() != 0 {
		t.Fatal("disabled service sent")
	}
}

func TestNotificationsUseOnlyTheCallersAccountAndAllowlist(t *testing.T) {
	n, sender, _ := setupNotifications(t)
	other := &fakeSender{}
	n.senders = fakeSenders{testAccount: sender, otherAccount: other}
	ctx := context.Background()

	if _, err := n.Send(ctx, otherAccount, releaseInput()); err == nil {
		t.Fatal("another account sent to a group allowed only by the test account")
	}
	if sender.calls.Load() != 0 || other.calls.Load() != 0 {
		t.Fatal("rejected cross-account request sent a message")
	}
	if err := n.store.AddNotificationChat(ctx, otherAccount, testGroup, "Releases", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(ctx, testAccount, releaseInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(ctx, otherAccount, releaseInput()); err != nil {
		t.Fatalf("same event id for another account must be independent: %v", err)
	}
	if sender.calls.Load() != 1 || other.calls.Load() != 1 {
		t.Fatalf("each account must send with its own session: test=%d other=%d", sender.calls.Load(), other.calls.Load())
	}
	if _, err := n.Send(ctx, 0, releaseInput()); err == nil {
		t.Fatal("request without account was accepted")
	}
}
