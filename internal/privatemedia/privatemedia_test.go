package privatemedia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
)

func newTestStore(t *testing.T, maxDisk int64) (*Store, *db.Store, *time.Time) {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/private-media.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cache, err := New(t.TempDir(), store, maxDisk)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	return cache, store, &now
}

func writeBytes(data []byte) func(context.Context, io.Writer) error {
	return func(_ context.Context, w io.Writer) error {
		_, err := w.Write(data)
		return err
	}
}

func item(owner int64, messageID int, size int64) db.PrivateMedia {
	return db.PrivateMedia{OwnerUserID: owner, MessageID: messageID, MediaID: int64(messageID) * 10, Kind: "voice", Size: size}
}

func TestSaveOpenMarkSentAndExpire(t *testing.T) {
	ctx := context.Background()
	cache, store, now := newTestStore(t, DefaultMaxDiskBytes)
	payload := []byte("voice bytes")
	if err := cache.Save(ctx, item(100, 7, int64(len(payload))), writeBytes(payload)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := cache.Open(ctx, 200, 7); err != nil || ok {
		t.Fatalf("another owner opened the attachment: ok=%v err=%v", ok, err)
	}
	got, file, ok, err := cache.Open(ctx, 100, 7)
	if err != nil || !ok || file == nil {
		t.Fatalf("open: ok=%v file=%v err=%v", ok, file, err)
	}
	data, _ := io.ReadAll(file)
	file.Close()
	if !bytes.Equal(data, payload) || got.Kind != "voice" || !got.ExpiresAt.Equal(now.Add(db.PrivateMediaTTL)) {
		t.Fatalf("cached = %#v data=%q", got, data)
	}

	if err := cache.MarkSent(ctx, 100, 7, "bot-file"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache.path(100, 7)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local copy kept after upload: %v", err)
	}
	got, file, ok, err = cache.Open(ctx, 100, 7)
	if err != nil || !ok || file != nil || got.BotFileID != "bot-file" {
		t.Fatalf("after upload: %#v file=%v ok=%v err=%v", got, file, ok, err)
	}

	*now = now.Add(db.PrivateMediaTTL + time.Second)
	if err := cache.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := cache.Open(ctx, 100, 7); err != nil || ok {
		t.Fatalf("expired attachment still available: ok=%v err=%v", ok, err)
	}
	if rows, err := store.ListExpiredPrivateMedia(ctx, now.Add(time.Hour), 10); err != nil || len(rows) != 0 {
		t.Fatalf("expired rows kept: %#v err=%v", rows, err)
	}
}

func TestDeletedMessageAttachmentIsHeldAndNotEvicted(t *testing.T) {
	ctx := context.Background()
	cache, store, now := newTestStore(t, MaxFileBytes)
	half := int64(MaxFileBytes / 2)
	if err := cache.Save(ctx, item(100, 1, half), writeBytes(make([]byte, half))); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPrivateMessageDeletions(ctx, 100, []int{1}, *now); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(ctx, item(100, 2, half), writeBytes(make([]byte, half))); err != nil {
		t.Fatal(err)
	}
	// The cache is full: the next file evicts message 2, never deleted message 1.
	if err := cache.Save(ctx, item(100, 3, half), writeBytes(make([]byte, half))); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := cache.Open(ctx, 100, 2); ok {
		t.Fatal("oldest undeleted attachment was not evicted")
	}
	if err := cache.Save(ctx, item(100, 4, half+1), writeBytes(make([]byte, half+1))); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("held attachment was evicted or limit ignored: %v", err)
	}

	*now = now.Add(2 * db.PrivateMediaTTL)
	if err := cache.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	held, file, ok, err := cache.Open(ctx, 100, 1)
	if err != nil || !ok || file == nil {
		t.Fatalf("deleted message attachment expired with the normal TTL: ok=%v err=%v", ok, err)
	}
	file.Close()
	if held.ExpiresAt.Before(now.Add(20 * time.Hour)) {
		t.Fatalf("held until %s", held.ExpiresAt)
	}
}

func TestSaveRejectsOversizeDownloadAndSweepsOrphans(t *testing.T) {
	ctx := context.Background()
	cache, _, _ := newTestStore(t, DefaultMaxDiskBytes)
	if err := cache.Save(ctx, item(100, 1, 4), writeBytes([]byte("too long"))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize download = %v, want ErrTooLarge", err)
	}
	if err := cache.Save(ctx, item(100, 2, MaxFileBytes+1), writeBytes(nil)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize announcement = %v, want ErrTooLarge", err)
	}
	orphan := filepath.Join(cache.dir, "100-9.bin")
	unrelated := filepath.Join(cache.dir, "keep.txt")
	for _, path := range []string{orphan, unrelated} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned cache file kept: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
	entries, _ := os.ReadDir(cache.dir)
	for _, entry := range entries {
		if entry.Name() != "keep.txt" {
			t.Fatalf("leftover file %s", entry.Name())
		}
	}
}
