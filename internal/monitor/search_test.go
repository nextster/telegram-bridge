package monitor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestParsePeerKey(t *testing.T) {
	peerType, peerID, err := parsePeerKey("channel:123")
	if err != nil {
		t.Fatal(err)
	}
	if peerType != "channel" || peerID != 123 {
		t.Fatalf("got %s:%d", peerType, peerID)
	}
	for _, value := range []string{"", "channel", "channel:nope", "channel:0"} {
		if _, _, err := parsePeerKey(value); err == nil {
			t.Fatalf("parsePeerKey(%q) succeeded", value)
		}
	}
}

func TestNormalizeLimit(t *testing.T) {
	if got := normalizeLimit(0, 20); got != 20 {
		t.Fatalf("default limit = %d", got)
	}
	if got := normalizeLimit(500, 20); got != maxQueryLimit {
		t.Fatalf("capped limit = %d", got)
	}
}

func TestMessagesFromResultMapsMetadata(t *testing.T) {
	reply := &tg.MessageReplyHeader{}
	reply.SetReplyToMsgID(41)
	reply.SetReplyToTopID(7)
	reply.SetForumTopic(true)

	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{Attributes: []tg.DocumentAttributeClass{
		&tg.DocumentAttributeVideo{},
		&tg.DocumentAttributeAnimated{},
	}})

	message := &tg.Message{
		ID:      42,
		PeerID:  &tg.PeerChat{ChatID: 99},
		Date:    1700000000,
		Message: "hello",
	}
	message.SetReplyTo(reply)
	message.SetMedia(media)
	message.SetEditDate(1700000100)

	messages, err := (&Service{}).messagesFromResult(context.Background(), 1, &tg.MessagesMessages{
		Messages: []tg.MessageClass{message},
		Chats:    []tg.ChatClass{&tg.Chat{ID: 99, Title: "Test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	got := messages[0]
	if got.ReplyToID != 41 || got.TopicID != 7 {
		t.Fatalf("reply metadata = (%d, %d), want (41, 7)", got.ReplyToID, got.TopicID)
	}
	if got.MediaKind != "animation" {
		t.Fatalf("media kind = %q, want animation", got.MediaKind)
	}
	wantEditedAt := time.Unix(1700000100, 0).UTC()
	if got.EditedAt == nil || !got.EditedAt.Equal(wantEditedAt) {
		t.Fatalf("edited at = %v, want %v", got.EditedAt, wantEditedAt)
	}
}

func TestTelegramReplyMetadata(t *testing.T) {
	regular := &tg.MessageReplyHeader{}
	regular.SetReplyToMsgID(11)

	forum := &tg.MessageReplyHeader{}
	forum.SetReplyToMsgID(21)
	forum.SetReplyToTopID(20)
	forum.SetForumTopic(true)

	directForum := &tg.MessageReplyHeader{}
	directForum.SetReplyToMsgID(30)
	directForum.SetForumTopic(true)

	nonForumThread := &tg.MessageReplyHeader{}
	nonForumThread.SetReplyToMsgID(41)
	nonForumThread.SetReplyToTopID(40)

	tests := []struct {
		name      string
		reply     tg.MessageReplyHeaderClass
		wantReply int
		wantTopic int
	}{
		{name: "absent"},
		{name: "regular reply", reply: regular, wantReply: 11},
		{name: "forum reply", reply: forum, wantReply: 21, wantTopic: 20},
		{name: "direct forum root reply", reply: directForum, wantReply: 30, wantTopic: 30},
		{name: "non-forum thread", reply: nonForumThread, wantReply: 41},
		{name: "story reply", reply: &tg.MessageReplyStoryHeader{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := &tg.Message{}
			if tt.reply != nil {
				message.SetReplyTo(tt.reply)
			}
			gotReply, gotTopic := telegramReplyMetadata(message)
			if gotReply != tt.wantReply || gotTopic != tt.wantTopic {
				t.Fatalf("got (%d, %d), want (%d, %d)", gotReply, gotTopic, tt.wantReply, tt.wantTopic)
			}
		})
	}
}

func TestTelegramEditedAt(t *testing.T) {
	if got := telegramEditedAt(&tg.Message{}); got != nil {
		t.Fatalf("absent edit date = %v, want nil", got)
	}
	zero := &tg.Message{}
	zero.SetEditDate(0)
	if got := telegramEditedAt(zero); got != nil {
		t.Fatalf("zero edit date = %v, want nil", got)
	}
	message := &tg.Message{}
	message.SetEditDate(1700000100)
	want := time.Unix(1700000100, 0).UTC()
	if got := telegramEditedAt(message); got == nil || !got.Equal(want) {
		t.Fatalf("edit date = %v, want %v", got, want)
	}
}

func TestTelegramMediaKind(t *testing.T) {
	document := func(attributes ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
		media := &tg.MessageMediaDocument{}
		media.SetDocument(&tg.Document{Attributes: attributes})
		return media
	}
	voice := &tg.DocumentAttributeAudio{}
	voice.SetVoice(true)
	videoNote := &tg.DocumentAttributeVideo{}
	videoNote.SetRoundMessage(true)
	tests := []struct {
		name  string
		media tg.MessageMediaClass
		want  string
	}{
		{name: "no media", want: "text"},
		{name: "empty", media: &tg.MessageMediaEmpty{}, want: "text"},
		{name: "photo", media: &tg.MessageMediaPhoto{}, want: "photo"},
		{name: "generic document", media: document(), want: "document"},
		{name: "voice", media: document(voice), want: "voice"},
		{name: "audio", media: document(&tg.DocumentAttributeAudio{}), want: "audio"},
		{name: "video note", media: document(videoNote), want: "video_note"},
		{name: "animation before video", media: document(&tg.DocumentAttributeVideo{}, &tg.DocumentAttributeAnimated{}), want: "animation"},
		{name: "sticker", media: document(&tg.DocumentAttributeSticker{}), want: "sticker"},
		{name: "custom emoji before sticker", media: document(&tg.DocumentAttributeSticker{}, &tg.DocumentAttributeCustomEmoji{}), want: "custom_emoji"},
		{name: "web page", media: &tg.MessageMediaWebPage{}, want: "web_page"},
		{name: "live location", media: &tg.MessageMediaGeoLive{}, want: "live_location"},
		{name: "poll", media: &tg.MessageMediaPoll{}, want: "poll"},
		{name: "unsupported", media: &tg.MessageMediaUnsupported{}, want: "unsupported"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := telegramMediaKind(tt.media); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTelegramMessageJSONContract(t *testing.T) {
	raw, err := json.Marshal(TelegramMessage{MediaKind: "text"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "chat", "date", "text", "media_kind"} {
		if _, ok := got[key]; !ok {
			t.Errorf("required key %q is absent from %s", key, raw)
		}
	}
	for _, key := range []string{"reply_to_id", "topic_id", "edited_at"} {
		if _, ok := got[key]; ok {
			t.Errorf("optional key %q unexpectedly present in %s", key, raw)
		}
	}
}
