package monitor

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/privatemedia"
)

const (
	privateMediaQueueSize       = 64
	privateMediaWorkers         = 2
	privateMediaDownloadTimeout = 3 * time.Minute
)

type privateMediaJob struct {
	media    db.PrivateMedia
	location tg.InputFileLocationClass
}

// SetPrivateMedia turns on caching of attachments from archived direct chats.
func (h *Handler) SetPrivateMedia(media *privatemedia.Store) {
	if media == nil {
		return
	}
	h.privateMedia = media
	h.mediaJobs = make(chan privateMediaJob, privateMediaQueueSize)
	h.mediaPending = make(map[int]time.Time)
}

// queuePrivateMedia schedules a download of a fresh attachment of an
// archived message. It never blocks update handling: a full queue drops it.
func (h *Handler) queuePrivateMedia(observed observedMessage) {
	if h.privateMedia == nil || observed.Media == nil {
		return
	}
	if time.Since(observed.MessageDate) > db.PrivateMediaTTL {
		return
	}
	item, location, ok := privateMediaFromMessage(observed.Media)
	if !ok {
		return
	}
	item.OwnerUserID = h.selfUserID.Load()
	item.MessageID = observed.MessageID

	h.mediaMu.Lock()
	if _, busy := h.mediaPending[item.MessageID]; busy {
		h.mediaMu.Unlock()
		return
	}
	h.mediaPending[item.MessageID] = time.Now()
	h.mediaMu.Unlock()

	select {
	case h.mediaJobs <- privateMediaJob{media: item, location: location}:
	default:
		h.finishPrivateMedia(item.MessageID)
		log.Printf("private media queue is full; skipped message %d", item.MessageID)
	}
}

func (h *Handler) finishPrivateMedia(messageID int) {
	h.mediaMu.Lock()
	delete(h.mediaPending, messageID)
	h.mediaMu.Unlock()
}

// privateMediaPending reports whether any of messages still has a download
// queued or running.
func (h *Handler) privateMediaPending(messages []db.PrivateMessage) bool {
	if h.privateMedia == nil {
		return false
	}
	h.mediaMu.Lock()
	defer h.mediaMu.Unlock()
	for id, since := range h.mediaPending {
		// A job lost to a reconnect must not hold alerts back forever.
		if time.Since(since) > privateMediaDownloadTimeout+privateDeletionMediaWait {
			delete(h.mediaPending, id)
		}
	}
	for _, message := range messages {
		if _, ok := h.mediaPending[message.MessageID]; ok {
			return true
		}
	}
	return false
}

// runPrivateMediaDownloads starts the download workers of one connection.
func (s *Service) runPrivateMediaDownloads(ctx context.Context) {
	if s.handler.privateMedia == nil {
		return
	}
	for range privateMediaWorkers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-s.handler.mediaJobs:
					s.downloadPrivateMedia(ctx, job)
					s.handler.finishPrivateMedia(job.media.MessageID)
				}
			}
		}()
	}
}

func (s *Service) downloadPrivateMedia(ctx context.Context, job privateMediaJob) {
	api, _, err := s.readyAPI()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, privateMediaDownloadTimeout)
	defer cancel()
	err = s.handler.privateMedia.Save(ctx, job.media, func(ctx context.Context, output io.Writer) error {
		_, err := downloader.NewDownloader().Download(api, job.location).Stream(ctx, output)
		return err
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("private media for message %d not cached: %v", job.media.MessageID, err)
	}
}

// privateMediaFromMessage describes an attachment the bot can send back,
// including view-once and forward-restricted media. Custom emoji are skipped.
func privateMediaFromMessage(media tg.MessageMediaClass) (db.PrivateMedia, tg.InputFileLocationClass, bool) {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return db.PrivateMedia{}, nil, false
		}
		size := largestPhotoSize(photo)
		if size.Type == "" || size.Bytes <= 0 || size.Bytes > privatemedia.MaxFileBytes {
			return db.PrivateMedia{}, nil, false
		}
		return db.PrivateMedia{
			MediaID: photo.ID, Kind: "photo", FileName: "photo.jpg", MIME: "image/jpeg",
			Size: size.Bytes, Width: size.W, Height: size.H,
		}, &tg.InputPhotoFileLocation{
			ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: size.Type,
		}, true
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok || doc.Size <= 0 || doc.Size > privatemedia.MaxFileBytes {
			return db.PrivateMedia{}, nil, false
		}
		kind := telegramMediaType(m)
		if telegramDocumentKind(m) == "custom_emoji" {
			return db.PrivateMedia{}, nil, false
		}
		item := db.PrivateMedia{MediaID: doc.ID, Kind: kind, MIME: doc.MimeType, Size: doc.Size}
		for _, attribute := range doc.Attributes {
			switch attribute := attribute.(type) {
			case *tg.DocumentAttributeFilename:
				item.FileName = attribute.FileName
			case *tg.DocumentAttributeAudio:
				item.Duration = attribute.Duration
			case *tg.DocumentAttributeVideo:
				item.Duration = int(attribute.Duration + 0.5)
				item.Width, item.Height = attribute.W, attribute.H
			}
		}
		return item, &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}, true
	default:
		return db.PrivateMedia{}, nil, false
	}
}

type photoSize struct {
	Type  string
	Bytes int64
	W, H  int
}

// largestPhotoSize picks the largest downloadable size of a photo.
func largestPhotoSize(photo *tg.Photo) photoSize {
	var best photoSize
	for _, size := range photo.Sizes {
		var current photoSize
		switch v := size.(type) {
		case *tg.PhotoSize:
			current = photoSize{Type: v.Type, Bytes: int64(v.Size), W: v.W, H: v.H}
		case *tg.PhotoSizeProgressive:
			current = photoSize{Type: v.Type, W: v.W, H: v.H}
			for _, part := range v.Sizes {
				if int64(part) > current.Bytes {
					current.Bytes = int64(part)
				}
			}
		default:
			continue
		}
		if current.Bytes > best.Bytes {
			best = current
		}
	}
	return best
}
