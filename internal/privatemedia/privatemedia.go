// Package privatemedia keeps short-lived copies of attachments from direct
// chats, so a deletion alert can include the file itself. A copy lives for
// db.PrivateMediaTTL; if its message is deleted, it is held until the owner
// has seen it, at most db.PrivateMediaDeletedHold. The metadata row decides
// what exists: files without a row are removed.
package privatemedia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
)

const (
	// MaxFileBytes is the Bot API upload limit; a larger file could not be
	// sent back anyway.
	MaxFileBytes = 50 << 20
	// DefaultMaxDiskBytes bounds the whole cache on the shared volume.
	DefaultMaxDiskBytes = 400 << 20

	cleanupInterval = time.Minute
	staleTempAge    = 10 * time.Minute
	tempPrefix      = ".download-"
	fileSuffix      = ".bin"
)

var (
	ErrTooLarge = errors.New("private media is larger than the cache allows")
	ErrNoSpace  = errors.New("private media cache is full")
)

type Store struct {
	dir          string
	store        *db.Store
	maxDiskBytes int64
	now          func() time.Time

	// mu serializes space accounting, the rename+row pair of a save and the
	// orphan sweep, so the sweep never sees a file whose row is not written yet.
	mu       sync.Mutex
	reserved int64
}

func New(dir string, store *db.Store, maxDiskBytes int64) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || store == nil {
		return nil, errors.New("private media directory and store are required")
	}
	if maxDiskBytes < MaxFileBytes {
		return nil, fmt.Errorf("private media disk limit must be at least %d bytes", MaxFileBytes)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create private media directory: %w", err)
	}
	return &Store{dir: dir, store: store, maxDiskBytes: maxDiskBytes, now: time.Now}, nil
}

func (s *Store) path(owner int64, messageID int) string {
	return filepath.Join(s.dir, fmt.Sprintf("%d-%d%s", owner, messageID, fileSuffix))
}

// Has reports whether this exact attachment is already cached.
func (s *Store) Has(ctx context.Context, owner int64, messageID int, mediaID int64) bool {
	item, ok, err := s.store.GetPrivateMedia(ctx, owner, messageID, s.now())
	return err == nil && ok && item.MediaID == mediaID
}

// Save downloads one attachment into the cache. item.Size is the size
// Telegram announced; the download may not write more than that.
func (s *Store) Save(ctx context.Context, item db.PrivateMedia, download func(context.Context, io.Writer) error) error {
	if item.Size <= 0 || item.Size > MaxFileBytes {
		return ErrTooLarge
	}
	if s.Has(ctx, item.OwnerUserID, item.MessageID, item.MediaID) {
		return nil
	}
	if err := s.reserve(ctx, item.Size); err != nil {
		return err
	}
	defer s.release(item.Size)

	tmp, err := os.CreateTemp(s.dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create private media file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	writer := &limitedWriter{w: tmp, remaining: item.Size}
	err = download(ctx, writer)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if writer.written == 0 {
		return errors.New("private media download was empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	final := s.path(item.OwnerUserID, item.MessageID)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("store private media file: %w", err)
	}
	now := s.now().UTC()
	item.Size = writer.written
	item.CreatedAt = now
	item.ExpiresAt = now.Add(db.PrivateMediaTTL)
	if err := s.store.SavePrivateMedia(ctx, item); err != nil {
		_ = os.Remove(final)
		return err
	}
	return nil
}

// Open returns the cached attachment of a message. The file is nil when the
// bot already has a file ID for it. The caller closes the file.
func (s *Store) Open(ctx context.Context, owner int64, messageID int) (db.PrivateMedia, *os.File, bool, error) {
	item, ok, err := s.store.GetPrivateMedia(ctx, owner, messageID, s.now())
	if err != nil || !ok {
		return db.PrivateMedia{}, nil, false, err
	}
	if item.BotFileID != "" {
		return item, nil, true, nil
	}
	file, err := os.Open(s.path(owner, messageID))
	if errors.Is(err, os.ErrNotExist) {
		return db.PrivateMedia{}, nil, false, s.store.DeletePrivateMedia(ctx, owner, messageID)
	}
	if err != nil {
		return db.PrivateMedia{}, nil, false, fmt.Errorf("open private media: %w", err)
	}
	return item, file, true, nil
}

// MarkSent stores the Bot API file ID and drops the local copy: from now on
// the bot resends the attachment from Telegram.
func (s *Store) MarkSent(ctx context.Context, owner int64, messageID int, fileID string) error {
	if strings.TrimSpace(fileID) == "" {
		return nil
	}
	if err := s.store.SetPrivateMediaBotFileID(ctx, owner, messageID, fileID); err != nil {
		return err
	}
	if err := os.Remove(s.path(owner, messageID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove sent private media: %w", err)
	}
	return nil
}

// Run removes expired and orphaned files until ctx ends.
func (s *Store) Run(ctx context.Context) error {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		if err := s.Cleanup(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("private media cleanup failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Store) Cleanup(ctx context.Context) error {
	for {
		expired, err := s.store.ListExpiredPrivateMedia(ctx, s.now(), 200)
		if err != nil {
			return err
		}
		for _, item := range expired {
			if err := s.remove(ctx, item); err != nil {
				return err
			}
		}
		if len(expired) < 200 {
			break
		}
	}
	return s.removeOrphans(ctx)
}

func (s *Store) remove(ctx context.Context, item db.PrivateMedia) error {
	if err := os.Remove(s.path(item.OwnerUserID, item.MessageID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove private media: %w", err)
	}
	return s.store.DeletePrivateMedia(ctx, item.OwnerUserID, item.MessageID)
}

func (s *Store) removeOrphans(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	local, err := s.store.ListLocalPrivateMedia(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(local))
	for _, item := range local {
		known[filepath.Base(s.path(item.OwnerUserID, item.MessageID))] = struct{}{}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read private media directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, tempPrefix) {
			// A download in progress owns its temp file; only stale ones go.
			if info, err := entry.Info(); err == nil && s.now().Sub(info.ModTime()) > staleTempAge {
				_ = os.Remove(filepath.Join(s.dir, name))
			}
			continue
		}
		if _, ok := known[name]; ok || !isCacheFileName(name) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphaned private media: %w", err)
		}
	}
	return nil
}

// reserve makes room for size bytes, evicting the oldest attachments of
// messages that were not deleted. Held attachments are never evicted.
func (s *Store) reserve(ctx context.Context, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	local, err := s.store.ListLocalPrivateMedia(ctx)
	if err != nil {
		return err
	}
	var used int64
	for _, item := range local {
		used += item.Size
	}
	if used+s.reserved+size > s.maxDiskBytes {
		evictable, err := s.store.ListEvictablePrivateMedia(ctx, 500)
		if err != nil {
			return err
		}
		for _, item := range evictable {
			if used+s.reserved+size <= s.maxDiskBytes {
				break
			}
			if err := s.remove(ctx, item); err != nil {
				return err
			}
			used -= item.Size
		}
	}
	if used+s.reserved+size > s.maxDiskBytes {
		return ErrNoSpace
	}
	s.reserved += size
	return nil
}

func (s *Store) release(size int64) {
	s.mu.Lock()
	s.reserved -= size
	s.mu.Unlock()
}

func isCacheFileName(name string) bool {
	base, ok := strings.CutSuffix(name, fileSuffix)
	if !ok {
		return false
	}
	owner, message, ok := strings.Cut(base, "-")
	if !ok {
		return false
	}
	_, ownerErr := strconv.ParseInt(owner, 10, 64)
	_, messageErr := strconv.Atoi(message)
	return ownerErr == nil && messageErr == nil
}

// limitedWriter refuses to write more than the announced size.
type limitedWriter struct {
	w         io.Writer
	remaining int64
	written   int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		return 0, ErrTooLarge
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	l.written += int64(n)
	return n, err
}
