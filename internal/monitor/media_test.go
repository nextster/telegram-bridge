package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/media"
)

type mediaRPC struct {
	message *tg.Message
	err     error
	calls   int
	data    []byte
	hashes  int
}

func (r *mediaRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	switch request := input.(type) {
	case *tg.MessagesGetMessagesRequest, *tg.ChannelsGetMessagesRequest:
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{r.message}}
	case *tg.UploadGetFileHashesRequest:
		r.hashes++
		return &tgerr.Error{Code: 400, Type: "LOCATION_INVALID"}
	case *tg.UploadGetFileRequest:
		location, ok := request.Location.(*tg.InputDocumentFileLocation)
		if !ok || location.ID != 123 || request.CDNSupported {
			return errors.New("unexpected download location or CDN request")
		}
		start := min(request.Offset, int64(len(r.data)))
		end := min(start+int64(request.Limit), int64(len(r.data)))
		output.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileMp3{}, Bytes: r.data[start:end]}
	default:
		return errors.New("unexpected RPC")
	}
	return nil
}

func TestDownloadDoesNotRequireOptionalServerHashes(t *testing.T) {
	for _, size := range []int{107590, 600000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			audio := &tg.DocumentAttributeAudio{Duration: 27}
			audio.SetVoice(true)
			rpc := &mediaRPC{data: bytes.Repeat([]byte("x"), size), message: &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 9}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 123, Size: int64(size), MimeType: "audio/ogg", Attributes: []tg.DocumentAttributeClass{audio}}}}}
			m := rpc.message.Media.(*tg.MessageMediaDocument)
			m.SetDocument(m.Document)
			s := &Service{cfg: config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"}, api: tg.NewClient(rpc), authorized: true, userID: 1}
			a, err := s.Attachment(context.Background(), "chat:9", 7)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := s.Download(context.Background(), a, &output); err != nil {
				t.Fatal(err)
			}
			if rpc.hashes != 0 || !bytes.Equal(output.Bytes(), rpc.data) {
				t.Fatal("download required optional hashes or changed original bytes")
			}
		})
	}
}

func TestAttachmentMessageScopeAndUnavailableAccount(t *testing.T) {
	rpc := &mediaRPC{message: &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 9}}}
	s := &Service{cfg: config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"}, api: tg.NewClient(rpc), authorized: true, userID: 1}
	if _, err := s.Attachment(context.Background(), "chat:8", 7); err == nil || err.Error() != "message_not_found" {
		t.Fatal("account-wide message ID crossed chat boundary")
	}
	s.authorized = false
	if _, err := s.Attachment(context.Background(), "chat:9", 7); err == nil {
		t.Fatal("unauthorized session accepted")
	}
	if rpc.calls != 1 {
		t.Fatal("RPC called without authorization")
	}
	if s.AccountID() != 0 {
		t.Fatal("unauthorized account exposed")
	}
}

func TestAttachmentKindsMetadataAndFingerprints(t *testing.T) {
	source := TelegramMessage{ID: 7, Chat: TelegramPeer{Key: "channel:42", ID: 42, Type: "channel", Username: "fixture"}, Sender: "Speaker", Date: time.Unix(1700000000, 0), ReplyToID: 6, TopicID: 2, MediaKind: "voice"}
	audio := &tg.DocumentAttributeAudio{Duration: 4}
	audio.SetVoice(true)
	doc := &tg.Document{ID: 123, AccessHash: 999, FileReference: []byte("sensitive-ref"), Size: 55, MimeType: "audio/ogg", Attributes: []tg.DocumentAttributeClass{audio}}
	m := &tg.MessageMediaDocument{Document: doc}
	reply := &tg.MessageReplyHeader{}
	reply.SetReplyToPeerID(&tg.PeerChannel{ChannelID: 10})
	msg := &tg.Message{ID: 7, Media: m, ReplyTo: reply}
	a, location := attachmentFromMessage(1, source, msg)
	if !a.Supported || a.Duration != 4 || a.Size != 55 || a.Author != "Speaker" || a.ReplyToID != 6 || a.ReplyToChat != "channel:10" || a.MessageURL != "https://t.me/fixture/7" || location == nil {
		t.Fatalf("bad attachment %+v", a)
	}
	doc.FileReference = []byte("refreshed")
	doc.AccessHash = 111
	b, _ := attachmentFromMessage(1, source, msg)
	if b.Fingerprint != a.Fingerprint {
		t.Fatal("ephemeral reference invalidated cache")
	}
	doc.ID++
	b, _ = attachmentFromMessage(1, source, msg)
	if b.Fingerprint == a.Fingerprint {
		t.Fatal("replacement media did not invalidate cache")
	}
	m.SetTTLSeconds(10)
	b, _ = attachmentFromMessage(1, source, msg)
	if b.Supported {
		t.Fatal("ephemeral media exported")
	}
	source.MediaKind = "video_note"
	video := &tg.DocumentAttributeVideo{Duration: 3}
	video.SetRoundMessage(true)
	msg.Media = &tg.MessageMediaDocument{Document: &tg.Document{ID: 2, Size: 99, MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{video}}}
	b, _ = attachmentFromMessage(1, source, msg)
	if !b.Supported || b.Duration != 3 {
		t.Fatal("video note not supported")
	}
}

func TestPhotoUsesLargestTelegramRepresentation(t *testing.T) {
	msg := &tg.Message{Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 99, AccessHash: 1, Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "m", Size: 100}, &tg.PhotoSizeProgressive{Type: "y", Sizes: []int{200, 800}}, &tg.PhotoStrippedSize{}}}}}
	a, location := attachmentFromMessage(1, TelegramMessage{ID: 1, Chat: TelegramPeer{Key: "chat:1"}, MediaKind: "photo"}, msg)
	if !a.Supported || a.Size != 800 || a.MIME != "image/jpeg" || location.(*tg.InputPhotoFileLocation).ThumbSize != "y" {
		t.Fatal("wrong photo size selected")
	}
	if a.MessageURL != "" {
		t.Fatal("invented legacy chat URL")
	}
}

func TestTelegramMediaErrorsAndSourceChange(t *testing.T) {
	for _, err := range []error{&tgerr.Error{Code: 420, Type: "FLOOD_WAIT", Argument: 17}, &tgerr.Error{Code: 500, Type: "INTERNAL"}, io.ErrUnexpectedEOF} {
		var f *media.Fault
		if !errors.As(mediaTelegramError(err), &f) || f.RetryAfter <= 0 {
			t.Fatal("transient error not retryable")
		}
	}
	if err := mediaTelegramError(&tgerr.Error{Code: 400, Type: "PRIVATE_SECRET"}); err.Error() != "telegram_access_or_message_error" {
		t.Fatal("leaked Telegram error")
	}
	rpc := &mediaRPC{message: &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 9}}}
	s := &Service{cfg: config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"}, api: tg.NewClient(rpc), authorized: true, userID: 1}
	err := s.Download(context.Background(), media.Attachment{AccountID: 1, Chat: "chat:9", MessageID: 7, Fingerprint: "old"}, io.Discard)
	if err == nil || err.Error() != "source_changed" || rpc.calls != 1 {
		t.Fatal("replacement attachment downloaded under old job")
	}
}
