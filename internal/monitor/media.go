package monitor

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/nextster/telegram-bridge/internal/media"
)

func (s *Service) AccountID() int64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.authorized || s.api == nil {
		return 0
	}
	return s.userID
}

func (s *Service) Attachment(ctx context.Context, chat string, id int) (media.Attachment, error) {
	a, _, err := s.attachment(ctx, chat, id)
	return a, err
}

func (s *Service) attachment(ctx context.Context, chat string, id int) (media.Attachment, tg.InputFileLocationClass, error) {
	if err := media.ValidateReference(chat, id); err != nil {
		return media.Attachment{}, nil, err
	}
	api, userID, err := s.readyAPI()
	if err != nil {
		return media.Attachment{}, nil, media.Retry("telegram_unavailable", 30*time.Second)
	}
	peer, err := s.resolvePeer(ctx, userID, chat)
	if err != nil {
		return media.Attachment{}, nil, media.Fail("unknown_chat")
	}
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var result tg.MessagesMessagesClass
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		result, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}, ID: ids})
	} else {
		result, err = api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return media.Attachment{}, nil, mediaTelegramError(err)
	}
	items, _, _ := messageParts(result)
	var msg *tg.Message
	for _, item := range items {
		m, ok := item.(*tg.Message)
		if !ok || m.ID != id {
			continue
		}
		pt, pid := peerInfo(m.PeerID)
		// Non-channel IDs are account-wide. Never accept a message from another chat.
		if peerKey(pt, pid) == chat {
			msg = m
			break
		}
	}
	if msg == nil {
		return media.Attachment{}, nil, media.Fail("message_not_found")
	}
	mapped, err := s.messagesFromResult(ctx, userID, result)
	if err != nil {
		return media.Attachment{}, nil, media.Fail("metadata_unavailable")
	}
	var source TelegramMessage
	for _, m := range mapped {
		if m.ID == id && m.Chat.Key == chat {
			source = m
			break
		}
	}
	a, location := attachmentFromMessage(userID, source, msg)
	return a, location, nil
}

func attachmentFromMessage(accountID int64, source TelegramMessage, msg *tg.Message) (media.Attachment, tg.InputFileLocationClass) {
	a := media.Attachment{AccountID: accountID, Chat: source.Chat.Key, MessageID: source.ID, Author: source.Sender, Date: source.Date, ReplyToID: source.ReplyToID, TopicID: source.TopicID, Kind: source.MediaKind}
	if a.Author == "" {
		a.Author = source.Chat.Title
	}
	if source.Chat.Type == "channel" {
		if source.Chat.Username != "" {
			a.MessageURL = fmt.Sprintf("https://t.me/%s/%d", source.Chat.Username, source.ID)
		} else {
			a.MessageURL = fmt.Sprintf("https://t.me/c/%d/%d", source.Chat.ID, source.ID)
		}
	}
	if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
		if p, ok := reply.GetReplyToPeerID(); ok {
			pt, pid := peerInfo(p)
			a.ReplyToChat = peerKey(pt, pid)
		}
	}
	var location tg.InputFileLocationClass
	var identity any
	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		if ttl, ok := m.GetTTLSeconds(); ok && ttl > 0 {
			return a, nil
		}
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return a, nil
		}
		a.MIME, a.Size = doc.MimeType, doc.Size
		for _, attr := range doc.Attributes {
			switch v := attr.(type) {
			case *tg.DocumentAttributeAudio:
				a.Duration = float64(v.Duration)
			case *tg.DocumentAttributeVideo:
				a.Duration = v.Duration
			}
		}
		a.Supported = a.Kind == "voice" || a.Kind == "video_note" || ((a.Kind == "document" || a.Kind == "image") && (a.MIME == "image/jpeg" || a.MIME == "image/png" || a.MIME == "image/webp"))
		if a.Supported && a.Kind == "document" {
			a.Kind = "image"
		}
		location = &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}
		identity = []any{"document", doc.ID, doc.Size, doc.MimeType, a.Duration}
	case *tg.MessageMediaPhoto:
		if ttl, ok := m.GetTTLSeconds(); ok && ttl > 0 {
			return a, nil
		}
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return a, nil
		}
		var sizeType string
		for _, size := range photo.Sizes {
			var n int
			var typ string
			switch v := size.(type) {
			case *tg.PhotoSize:
				n, typ = v.Size, v.Type
			case *tg.PhotoSizeProgressive:
				for _, part := range v.Sizes {
					if part > n {
						n = part
					}
				}
				typ = v.Type
			}
			if int64(n) > a.Size {
				a.Size, sizeType = int64(n), typ
			}
		}
		if sizeType == "" {
			return a, nil
		}
		a.MIME, a.Supported = "image/jpeg", true
		location = &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: sizeType}
		identity = []any{"photo", photo.ID, sizeType, a.Size}
	}
	// Telegram document/photo IDs identify immutable media. Expiring file references
	// and caption edits deliberately do not invalidate a paid recognition result.
	if identity != nil {
		a.Fingerprint = media.Digest(identity)
	}
	return a, location
}

func (s *Service) Download(ctx context.Context, expected media.Attachment, output io.Writer) error {
	current, location, err := s.attachment(ctx, expected.Chat, expected.MessageID)
	if err != nil {
		return err
	}
	if current.AccountID != expected.AccountID || current.Fingerprint != expected.Fingerprint {
		return media.Fail("source_changed")
	}
	if !current.Supported || location == nil {
		return media.Fail("unsupported_attachment")
	}
	api, userID, err := s.readyAPI()
	if err != nil {
		return media.Retry("telegram_unavailable", 30*time.Second)
	}
	if userID != expected.AccountID {
		return media.Fail("telegram_account_changed")
	}
	_, err = downloader.NewDownloader().Download(api, location).WithVerify(true).Stream(ctx, output)
	if tgerr.Is(err, "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID") {
		return media.Retry("telegram_file_reference_expired", time.Second)
	}
	if err != nil {
		return mediaTelegramError(err)
	}
	return nil
}

func mediaTelegramError(err error) error {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return media.Retry("telegram_flood_wait", max(d, time.Second))
	}
	if rpc, ok := tgerr.As(err); ok {
		if rpc.Code >= 500 {
			return media.Retry("telegram_server_error", 15*time.Second)
		}
		return media.Fail("telegram_access_or_message_error")
	}
	return media.Retry("telegram_download_failed", 15*time.Second)
}
