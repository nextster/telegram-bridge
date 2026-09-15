package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
)

type Download struct {
	FileID      string     `json:"file_id"`
	DownloadURL string     `json:"download_url"`
	SHA256      string     `json:"sha256"`
	Size        int64      `json:"size_bytes"`
	MIME        string     `json:"mime_type"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Source      Attachment `json:"source"`
}

func (s *Service) Download(ctx context.Context, accountID int64, chat string, id int) (Download, error) {
	a, err := s.Metadata(ctx, accountID, chat, id)
	if err != nil {
		return Download{}, err
	}
	if err := s.checkAttachment(a, ""); err != nil {
		return Download{}, err
	}
	select {
	case s.files <- struct{}{}:
		defer func() { <-s.files }()
	default:
		return Download{}, Retry("download_busy", time.Second)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	f, err := s.downloadLocked(ctx, a)
	if err != nil {
		return Download{}, err
	}
	return Download{FileID: f.ID, SHA256: f.SHA256, Size: f.Size, MIME: f.MIME, ExpiresAt: time.Unix(f.ExpiresAt, 0).UTC(), Source: a}, nil
}

func (s *Service) downloadLocked(ctx context.Context, a Attachment) (db.MediaFile, error) {
	// Refresh metadata even for cached downloads, including authorization and edits.
	current, err := s.source.Attachment(ctx, a.AccountID, a.Chat, a.MessageID)
	if err != nil {
		return db.MediaFile{}, fault(err)
	}
	if current.AccountID != a.AccountID || current.Fingerprint != a.Fingerprint {
		return db.MediaFile{}, Fail("source_changed")
	}
	id := Digest([]any{a.AccountID, a.Chat, a.MessageID, a.Fingerprint})
	if f, err := s.store.MediaFile(ctx, id); err == nil && f.ExpiresAt > s.now().Unix() {
		if info, err := s.root.Lstat(id + ".bin"); err == nil && info.Mode().IsRegular() && info.Size() == f.Size {
			file, err := s.root.Open(id + ".bin")
			if err != nil {
				return db.MediaFile{}, Fail("file_unavailable")
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, file)
			file.Close()
			if copyErr == nil && hex.EncodeToString(h.Sum(nil)) == f.SHA256 {
				return f, nil
			}
			return db.MediaFile{}, Fail("cached_file_integrity_error")
		}
	}
	if err := s.cleanupLocked(ctx, false); err != nil {
		return db.MediaFile{}, err
	}
	list, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return db.MediaFile{}, Fail("file_storage_unavailable")
	}
	var used int64
	for _, entry := range list {
		info, err := entry.Info()
		if err != nil {
			return db.MediaFile{}, Fail("file_storage_unavailable")
		}
		if info.Mode().IsRegular() {
			used += info.Size()
		}
	}
	// Reserve additional room for normalization; the file lock serializes writers.
	if used+a.Size+(8<<20) > s.cfg.MaxDiskBytes {
		return db.MediaFile{}, Fail("file_storage_limit")
	}
	name := "tmp-" + id
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return db.MediaFile{}, Fail("file_storage_unavailable")
	}
	defer s.root.Remove(name)
	h := sha256.New()
	w := &limitedWriter{target: io.MultiWriter(file, h), remaining: min(a.Size, s.cfg.MaxBytes)}
	err = s.source.Download(ctx, a.AccountID, a, w)
	syncErr := file.Sync()
	closeErr := file.Close()
	if w.exceeded {
		return db.MediaFile{}, Fail("attachment_size_limit")
	}
	if err != nil {
		return db.MediaFile{}, fault(err)
	}
	if w.remaining != 0 {
		return db.MediaFile{}, Retry("incomplete_download", time.Second)
	}
	if syncErr != nil || closeErr != nil {
		return db.MediaFile{}, Fail("file_storage_unavailable")
	}
	if err := s.root.Rename(name, id+".bin"); err != nil {
		return db.MediaFile{}, Fail("file_storage_unavailable")
	}
	f := db.MediaFile{ID: id, AccountID: a.AccountID, Chat: a.Chat, MessageID: a.MessageID, Fingerprint: a.Fingerprint, Size: a.Size, SHA256: hex.EncodeToString(h.Sum(nil)), MIME: a.MIME, ExpiresAt: s.now().Add(time.Duration(s.cfg.RetentionHours) * time.Hour).Unix()}
	if err := s.store.PutMediaFile(ctx, f); err != nil {
		s.root.Remove(id + ".bin")
		return db.MediaFile{}, Fail("storage_unavailable")
	}
	return f, nil
}

type limitedWriter struct {
	target    io.Writer
	remaining int64
	exceeded  bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.exceeded = true
		return 0, Fail("attachment_size_limit")
	}
	n, err := w.target.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// OpenDownload opens a cached original only for the account that downloaded it.
func (s *Service) OpenDownload(ctx context.Context, accountID int64, id string) (*os.File, db.MediaFile, error) {
	if !idPattern.MatchString(id) {
		return nil, db.MediaFile{}, Fail("file_not_found")
	}
	f, err := s.store.MediaFile(ctx, id)
	if err != nil || accountID <= 0 || f.AccountID != accountID || f.ExpiresAt <= s.now().Unix() {
		return nil, db.MediaFile{}, Fail("file_not_found")
	}
	a, err := s.Metadata(ctx, accountID, f.Chat, f.MessageID)
	if err != nil || a.AccountID != f.AccountID || a.Fingerprint != f.Fingerprint {
		return nil, db.MediaFile{}, Fail("file_not_found")
	}
	info, err := s.root.Lstat(id + ".bin")
	if err != nil || !info.Mode().IsRegular() {
		return nil, db.MediaFile{}, Fail("file_not_found")
	}
	file, err := s.root.Open(id + ".bin")
	if err != nil {
		return nil, db.MediaFile{}, Fail("file_not_found")
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, io.LimitReader(file, s.cfg.MaxBytes+1))
	if copyErr != nil || n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		file.Close()
		return nil, db.MediaFile{}, Fail("file_integrity_error")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, db.MediaFile{}, Fail("file_unavailable")
	}
	return file, f, nil
}

func (s *Service) cleanup(ctx context.Context, startup bool) error {
	select {
	case s.files <- struct{}{}:
		defer func() { <-s.files }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.cleanupLocked(ctx, startup)
}

func (s *Service) cleanupLocked(ctx context.Context, startup bool) error {
	entries, err := os.ReadDir(s.cfg.Directory)
	if err != nil {
		return Fail("file_cleanup_failed")
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "tmp-") {
			if startup {
				if err := s.root.RemoveAll(name); err != nil {
					return Fail("file_cleanup_failed")
				}
			}
			continue
		}
		id := strings.TrimSuffix(name, ".bin")
		if id == name || !idPattern.MatchString(id) {
			continue
		}
		f, err := s.store.MediaFile(ctx, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Fail("file_cleanup_failed")
		}
		if errors.Is(err, sql.ErrNoRows) || f.ExpiresAt <= s.now().Unix() {
			if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return Fail("file_cleanup_failed")
			}
		}
	}
	if err := s.store.PruneMediaFiles(ctx, s.now().Unix()); err != nil {
		return Fail("file_cleanup_failed")
	}
	return nil
}
