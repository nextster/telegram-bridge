package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/privatemedia"
)

func voiceMedia(id int64, size int64) *tg.MessageMediaDocument {
	audio := &tg.DocumentAttributeAudio{Voice: true, Duration: 12}
	audio.SetFlags()
	return documentMedia(&tg.Document{
		ID: id, AccessHash: 7, FileReference: []byte{1}, MimeType: "audio/ogg", Size: size,
		Attributes: []tg.DocumentAttributeClass{audio},
	})
}

// documentMedia sets the TL flags a decoded update would carry.
func documentMedia(document *tg.Document) *tg.MessageMediaDocument {
	media := &tg.MessageMediaDocument{Document: document}
	media.SetFlags()
	return media
}

func TestPrivateMediaFromMessageKeepsSendableAttachmentsOnly(t *testing.T) {
	item, location, ok := privateMediaFromMessage(voiceMedia(42, 2048))
	if !ok || item.Kind != "voice" || item.Duration != 12 || item.Size != 2048 || item.MediaID != 42 {
		t.Fatalf("voice = %#v ok=%v", item, ok)
	}
	if loc, isDoc := location.(*tg.InputDocumentFileLocation); !isDoc || loc.ID != 42 {
		t.Fatalf("voice location = %#v", location)
	}

	video := &tg.DocumentAttributeVideo{RoundMessage: true, Duration: 9.6, W: 384, H: 384}
	video.SetFlags()
	round := documentMedia(&tg.Document{ID: 43, MimeType: "video/mp4", Size: 4096,
		Attributes: []tg.DocumentAttributeClass{video}})
	if item, _, ok := privateMediaFromMessage(round); !ok || item.Kind != "video_note" || item.Duration != 10 || item.Width != 384 {
		t.Fatalf("video note = %#v ok=%v", item, ok)
	}

	photo := &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 44, Sizes: []tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 100},
		&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 960, Sizes: []int{500, 900}},
	}}}
	item, location, ok = privateMediaFromMessage(photo)
	if !ok || item.Kind != "photo" || item.Size != 900 || item.Width != 1280 {
		t.Fatalf("photo = %#v ok=%v", item, ok)
	}
	if loc, isPhoto := location.(*tg.InputPhotoFileLocation); !isPhoto || loc.ThumbSize != "y" {
		t.Fatalf("photo location = %#v", location)
	}

	viewOnce := &tg.MessageMediaPhoto{Photo: photo.Photo}
	viewOnce.SetTTLSeconds(10)
	if item, _, ok := privateMediaFromMessage(viewOnce); !ok || item.Kind != "photo" {
		t.Fatalf("view-once photo = %#v ok=%v", item, ok)
	}

	emoji := documentMedia(&tg.Document{ID: 45, Size: 10,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeCustomEmoji{}}})
	for name, media := range map[string]tg.MessageMediaClass{
		"custom emoji": emoji,
		"too large":    voiceMedia(46, privatemedia.MaxFileBytes+1),
		"web page":     &tg.MessageMediaWebPage{},
		"no media":     nil,
	} {
		if item, _, ok := privateMediaFromMessage(media); ok {
			t.Fatalf("%s was accepted: %#v", name, item)
		}
	}
}

func newMediaHandler(t *testing.T) (*Handler, *db.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/private-media.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	media, err := privatemedia.New(t.TempDir(), store, privatemedia.DefaultMaxDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(store, &captureNotifier{})
	handler.SetSelfUserID(100)
	handler.SetPrivateMedia(media)
	return handler, store
}

func queuedMessageIDs(handler *Handler) []int {
	var ids []int
	for {
		select {
		case job := <-handler.mediaJobs:
			ids = append(ids, job.media.MessageID)
		default:
			return ids
		}
	}
}

func TestHandlerQueuesFreshAttachmentsOfDirectChatsOnly(t *testing.T) {
	ctx := context.Background()
	handler, _ := newMediaHandler(t)
	now := int(time.Now().Unix())
	users := []tg.UserClass{
		&tg.User{ID: 200, AccessHash: 1, FirstName: "Alice"},
		&tg.User{ID: 300, AccessHash: 2, FirstName: "Robot", Bot: true},
	}
	protected := &tg.Message{ID: 4, PeerID: &tg.PeerUser{UserID: 200}, FromID: &tg.PeerUser{UserID: 200}, Date: now, Media: voiceMedia(4, 10)}
	protected.Noforwards = true
	messages := []*tg.Message{
		{ID: 1, PeerID: &tg.PeerUser{UserID: 200}, FromID: &tg.PeerUser{UserID: 200}, Date: now, Media: voiceMedia(1, 10)},
		{ID: 2, PeerID: &tg.PeerUser{UserID: 200}, FromID: &tg.PeerUser{UserID: 200}, Date: now - 2*3600, Media: voiceMedia(2, 10)},
		{ID: 3, PeerID: &tg.PeerUser{UserID: 300}, FromID: &tg.PeerUser{UserID: 300}, Date: now, Media: voiceMedia(3, 10)},
		protected,
		{ID: 5, PeerID: &tg.PeerChat{ChatID: 900}, FromID: &tg.PeerUser{UserID: 200}, Date: now, Media: voiceMedia(5, 10)},
		{ID: 6, PeerID: &tg.PeerUser{UserID: 200}, Out: true, Date: now, Media: voiceMedia(6, 10)},
	}
	updates := make([]tg.UpdateClass, 0, len(messages))
	for _, message := range messages {
		updates = append(updates, &tg.UpdateNewMessage{Message: message})
	}
	if err := handler.Handle(ctx, &tg.Updates{Users: users, Updates: updates}); err != nil {
		t.Fatal(err)
	}
	got := queuedMessageIDs(handler)
	if len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 6 {
		t.Fatalf("queued = %v, want fresh direct-chat attachments [1 4 6]", got)
	}
}

func TestPrivateDeletionOutboxWaitsForAttachmentDownload(t *testing.T) {
	ctx := context.Background()
	handler, _ := newMediaHandler(t)
	notifier := &captureNotifier{}
	handler.deletionNotifier = notifier
	handler.deletionQuiet = 0
	users := []tg.UserClass{&tg.User{ID: 200, AccessHash: 1, FirstName: "Alice"}}
	if err := handler.Handle(ctx, &tg.Updates{Users: users, Updates: []tg.UpdateClass{&tg.UpdateNewMessage{Message: &tg.Message{
		ID: 9, PeerID: &tg.PeerUser{UserID: 200}, FromID: &tg.PeerUser{UserID: 200}, Date: int(time.Now().Unix()), Media: voiceMedia(9, 10),
	}}}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: []int{9}}}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notifier.deletions) != 0 {
		t.Fatal("alert sent while its attachment was still downloading")
	}
	<-handler.mediaJobs
	handler.finishPrivateMedia(9)
	if err := handler.flushPrivateDeletionOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notifier.deletions) != 1 || notifier.deletions[0].Messages[0].MediaType != "voice" {
		t.Fatalf("alerts = %#v, want one voice deletion", notifier.deletions)
	}
}
