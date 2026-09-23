package bot

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/privatemedia"
)

const friendPeer = int64(7)

// archiveDeleted stores messages of alice's chat with Friend as deleted and
// returns the alert that covers them.
func archiveDeleted(t *testing.T, store *db.Store, messages ...db.PrivateMessage) db.PrivateMessageDeletion {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertPrivateDialog(ctx, db.PrivateDialog{OwnerUserID: aliceUser, PeerID: friendPeer, Title: "Friend", Username: "friend"}); err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 0, len(messages))
	for i, message := range messages {
		message.OwnerUserID, message.PeerID = aliceUser, friendPeer
		message.MessageDate = time.Unix(int64(1000+i), 0).UTC()
		if err := store.UpsertPrivateMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, message.MessageID)
	}
	if err := store.RecordPrivateMessageDeletions(ctx, aliceUser, ids, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	alertID, err := store.CreatePrivateDeletionAlert(ctx, aliceUser, friendPeer, ids, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	deletion, ok, err := store.GetPrivateDeletionAlert(ctx, aliceUser, alertID)
	if err != nil || !ok {
		t.Fatalf("alert: ok=%v err=%v", ok, err)
	}
	return deletion
}

func newDeletionTestService(t *testing.T) (*Service, *db.Store, *fakeTelegram) {
	t.Helper()
	service, store, api := newOAuthTestService(t)
	for _, user := range []int64{aliceUser, bobUser} {
		if err := store.UpsertSubscriber(context.Background(), db.Subscriber{ChatID: user}); err != nil {
			t.Fatal(err)
		}
	}
	return service, store, api
}

func callbackData(t *testing.T, call telegramCall) string {
	t.Helper()
	markup, _ := call.Body["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("markup = %#v, want one button", call.Body["reply_markup"])
	}
	buttons, _ := rows[0].([]any)
	button, _ := buttons[0].(map[string]any)
	data, _ := button["callback_data"].(string)
	return data
}

func methods(calls []telegramCall) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.Method)
	}
	return out
}

func TestDeletionSummaryShowsMessagesOnlyToItsOwner(t *testing.T) {
	ctx := context.Background()
	service, store, api := newDeletionTestService(t)
	deletion := archiveDeleted(t, store,
		db.PrivateMessage{MessageID: 1, Text: "hi"},
		db.PrivateMessage{MessageID: 2, Text: "hello", Outgoing: true},
		db.PrivateMessage{MessageID: 3, Text: "look", MediaType: "photo"},
	)
	if err := service.NotifyDeletedMessages(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if len(calls) != 1 || calls[0].Method != "sendMessage" || calls[0].Body["chat_id"] != float64(aliceUser) {
		t.Fatalf("summary calls = %#v", calls)
	}
	if text := calls[0].Body["text"]; text != "🫥 Friend: удалено 3 сообщения (ваших: 1)" {
		t.Fatalf("summary = %q", text)
	}
	data := callbackData(t, calls[0])
	if data != fmt.Sprintf("delshow:%d", deletion.AlertID) {
		t.Fatalf("button data = %q", data)
	}

	if err := service.handleUpdate(ctx, oauthCallback(bobUser, bobUser, data)); err != nil {
		t.Fatal(err)
	}
	if sent := sentTo(api.take()); len(sent) != 0 {
		t.Fatalf("bob's press revealed alice's messages: %#v", sent)
	}

	if err := service.handleUpdate(ctx, oauthCallback(aliceUser, aliceUser, data)); err != nil {
		t.Fatal(err)
	}
	sent := sentTo(api.take())
	want := "Friend: hi\n\nВы: hello\n\nFriend: [фото] look"
	if len(sent) != 1 || len(sent[float64(aliceUser)]) != 1 || sent[float64(aliceUser)][0] != want {
		t.Fatalf("shown = %#v, want %q", sent, want)
	}
}

func TestDeletionShowPagesLongAlerts(t *testing.T) {
	ctx := context.Background()
	service, store, api := newDeletionTestService(t)
	messages := make([]db.PrivateMessage, 35)
	for i := range messages {
		messages[i] = db.PrivateMessage{MessageID: i + 1, Text: fmt.Sprintf("line %d", i+1)}
	}
	deletion := archiveDeleted(t, store, messages...)

	if err := service.handleUpdate(ctx, oauthCallback(aliceUser, aliceUser, fmt.Sprintf("delshow:%d", deletion.AlertID))); err != nil {
		t.Fatal(err)
	}
	var shown []telegramCall
	for _, call := range api.take() {
		if call.Method == "sendMessage" {
			shown = append(shown, call)
		}
	}
	last := shown[len(shown)-1]
	if last.Body["text"] != "Показано 30 из 35." || callbackData(t, last) != fmt.Sprintf("delshow:%d:30", deletion.AlertID) {
		t.Fatalf("page footer = %#v", last.Body)
	}
	if text, _ := shown[0].Body["text"].(string); !strings.HasPrefix(text, "Friend: line 1\n\n") || strings.Contains(text, "line 31") {
		t.Fatalf("first page = %q", text)
	}

	if err := service.handleUpdate(ctx, oauthCallback(aliceUser, aliceUser, fmt.Sprintf("delshow:%d:30", deletion.AlertID))); err != nil {
		t.Fatal(err)
	}
	sent := sentTo(api.take())[float64(aliceUser)]
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "Friend: line 31") || !strings.HasSuffix(sent[0], "Friend: line 35") {
		t.Fatalf("second page = %#v", sent)
	}
}

func newCachedMedia(t *testing.T, store *db.Store, service *Service, item db.PrivateMedia, data string) *privatemedia.Store {
	t.Helper()
	cache, err := privatemedia.New(t.TempDir(), store, privatemedia.DefaultMaxDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	item.OwnerUserID, item.Size = aliceUser, int64(len(data))
	if err := cache.Save(context.Background(), item, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	service.SetPrivateMedia(cache)
	return cache
}

func TestDeletedVoiceIsSentFromCacheThenByFileID(t *testing.T) {
	ctx := context.Background()
	service, store, api := newDeletionTestService(t)
	cache := newCachedMedia(t, store, service, db.PrivateMedia{MessageID: 10, MediaID: 1, Kind: "voice", Duration: 3}, "OggS voice")
	deletion := archiveDeleted(t, store, db.PrivateMessage{MessageID: 10, MediaType: "voice"})

	if err := service.NotifyDeletedMessages(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if len(calls) != 1 || calls[0].Method != "sendVoice" {
		t.Fatalf("calls = %v, want one sendVoice", methods(calls))
	}
	body := calls[0].Body
	if body["chat_id"] != fmt.Sprint(aliceUser) || body["voice"] != "upload:voice.ogg:OggS voice" ||
		body["caption"] != "🫥 Friend: удалено сообщение" {
		t.Fatalf("sendVoice body = %#v", body)
	}
	item, file, ok, err := cache.Open(ctx, aliceUser, 10)
	if err != nil || !ok || file != nil || item.BotFileID != "sendVoice-file" {
		t.Fatalf("after upload: %#v file=%v ok=%v err=%v", item, file, ok, err)
	}

	// Showing it again reuses the uploaded file instead of the disk copy.
	if err := service.handleUpdate(ctx, oauthCallback(aliceUser, aliceUser, fmt.Sprintf("delshow:%d", deletion.AlertID))); err != nil {
		t.Fatal(err)
	}
	var voice map[string]any
	for _, call := range api.take() {
		if call.Method == "sendVoice" {
			voice = call.Body
		}
	}
	if voice["voice"] != "sendVoice-file" || voice["caption"] != "Friend:" {
		t.Fatalf("resend body = %#v", voice)
	}
}

func TestDeletedVideoNoteSendsCaptionAsText(t *testing.T) {
	ctx := context.Background()
	service, store, api := newDeletionTestService(t)
	newCachedMedia(t, store, service, db.PrivateMedia{MessageID: 20, MediaID: 2, Kind: "video_note", Width: 384}, "mp4")
	deletion := archiveDeleted(t, store, db.PrivateMessage{MessageID: 20, MediaType: "video_note", Outgoing: true})

	if err := service.NotifyDeletedMessages(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if got := methods(calls); len(got) != 2 || got[0] != "sendMessage" || got[1] != "sendVideoNote" {
		t.Fatalf("calls = %v, want text then video note", got)
	}
	if calls[0].Body["text"] != "🫥 Friend: удалено ваше сообщение" || calls[1].Body["length"] != "384" {
		t.Fatalf("bodies = %#v / %#v", calls[0].Body, calls[1].Body)
	}
}

func TestDeletedMediaWithoutCacheFallsBackToText(t *testing.T) {
	ctx := context.Background()
	service, store, api := newDeletionTestService(t)
	newCachedMedia(t, store, service, db.PrivateMedia{MessageID: 99, MediaID: 9, Kind: "photo"}, "jpg")
	deletion := archiveDeleted(t, store, db.PrivateMessage{MessageID: 30, MediaType: "photo", Text: "caption"})

	if err := service.NotifyDeletedMessages(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if len(calls) != 1 || calls[0].Body["text"] != "🫥 Friend: удалено сообщение\n\n[фото] caption" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestFormatDeletionSummaryPlurals(t *testing.T) {
	for count, want := range map[int]string{2: "2 сообщения", 5: "5 сообщений", 11: "11 сообщений", 21: "21 сообщение", 104: "104 сообщения"} {
		messages := make([]db.PrivateMessage, count)
		got := formatDeletionSummary(db.PrivateMessageDeletion{Dialog: db.PrivateDialog{PeerID: 7, Title: "Friend"}, Messages: messages})
		if got != "🫥 Friend: удалено "+want {
			t.Fatalf("summary for %d = %q", count, got)
		}
	}
}

func TestShowKeepsTextTogetherAroundExpiredAttachments(t *testing.T) {
	ctx := context.Background()
	service, store, api := newDeletionTestService(t)
	newCachedMedia(t, store, service, db.PrivateMedia{MessageID: 99, MediaID: 9, Kind: "photo"}, "jpg")
	deletion := archiveDeleted(t, store,
		db.PrivateMessage{MessageID: 1, Text: "before"},
		db.PrivateMessage{MessageID: 2, MediaType: "photo"},
		db.PrivateMessage{MessageID: 3, Text: "after"},
	)
	if err := service.handleUpdate(ctx, oauthCallback(aliceUser, aliceUser, fmt.Sprintf("delshow:%d", deletion.AlertID))); err != nil {
		t.Fatal(err)
	}
	sent := sentTo(api.take())[float64(aliceUser)]
	if len(sent) != 1 || sent[0] != "Friend: before\n\nFriend: [фото]\n\nFriend: after" {
		t.Fatalf("shown = %#v, want one text message", sent)
	}
}
