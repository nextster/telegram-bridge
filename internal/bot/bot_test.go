package bot

import (
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/nextster/telegram-bridge/internal/db"
)

func TestMessageURLFromPeer(t *testing.T) {
	tests := []struct {
		name  string
		event db.Event
		peer  db.MonitorPeer
		want  string
	}{
		{
			name: "public username",
			event: db.Event{
				SourcePeerType: "channel",
				SourcePeerID:   1000000001,
				MessageID:      42,
			},
			peer: db.MonitorPeer{
				Username: "@radar_chat",
			},
			want: "https://t.me/radar_chat/42",
		},
		{
			name: "private channel id",
			event: db.Event{
				SourcePeerType: "channel",
				SourcePeerID:   1000000001,
				MessageID:      42,
			},
			want: "https://t.me/c/1000000001/42",
		},
		{
			name: "bot api channel id",
			event: db.Event{
				SourcePeerType: "channel",
				SourcePeerID:   -1001000000001,
				MessageID:      42,
			},
			want: "https://t.me/c/1000000001/42",
		},
		{
			name: "user chat without username",
			event: db.Event{
				SourcePeerType: "user",
				SourcePeerID:   123,
				MessageID:      42,
			},
			want: "",
		},
		{
			name: "missing message id",
			event: db.Event{
				SourcePeerType: "channel",
				SourcePeerID:   1000000001,
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messageURLFromPeer(tt.event, tt.peer); got != tt.want {
				t.Fatalf("messageURLFromPeer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDeletedMessagesIsClearAndBounded(t *testing.T) {
	messages := make([]db.PrivateMessage, 8)
	for i := range messages {
		messages[i] = db.PrivateMessage{
			MessageID: i + 1, PeerID: 200, MessageDate: time.Unix(int64(100+i), 0).UTC(),
			Text: strings.Repeat("длинный текст ", 100), Outgoing: i%2 == 0,
		}
	}
	body := formatDeletedMessages(db.PrivateMessageDeletion{
		Dialog:   db.PrivateDialog{PeerID: 200, Title: "Alice Example", Username: "alice"},
		Messages: messages,
	})
	for _, want := range []string{"🫥 Удалено из: Alice Example (@alice) · 8 сообщений", "1. длинный текст", "…и ещё 2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body does not contain %q: %q", want, body)
		}
	}
	for _, noise := range []string{"Telegram не сообщает", "входящее", "исходящее", "1970-01-01T"} {
		if strings.Contains(body, noise) {
			t.Fatalf("body contains noisy metadata %q: %q", noise, body)
		}
	}
	if got := len(utf16.Encode([]rune(body))); got > 3900 {
		t.Fatalf("body length = %d UTF-16 units", got)
	}
}

func TestFormatDeletedMessageIsJustChatAndContent(t *testing.T) {
	body := formatDeletedMessages(db.PrivateMessageDeletion{
		Dialog: db.PrivateDialog{PeerID: 200, Title: "Алиса Пример", Username: "alice_example"},
		Messages: []db.PrivateMessage{{
			MessageID:   12,
			PeerID:      200,
			MessageDate: time.Date(2026, 8, 17, 10, 50, 21, 0, time.UTC),
			Text:        "Созвонимся завтра?",
		}},
	})
	want := "🫥 Удалено из: Алиса Пример (@alice_example)\n\nСозвонимся завтра?"
	if body != want {
		t.Fatalf("formatDeletedMessages() = %q, want %q", body, want)
	}
}

func TestFormatEventIsConcise(t *testing.T) {
	got := formatEvent(db.Event{
		Keyword:     "exact cassette",
		MatchReason: "any: cassette; required: xg-1250, 10-36, xdr",
		MatchScore:  130,
		RuleNote:    "Verify tooth wear and XDR compatibility.",
		Text:        "  SRAM XG-1250 cassette for sale  ",
	})
	if got != "🔎 Найдено:\n\nSRAM XG-1250 cassette for sale" {
		t.Fatalf("formatEvent() = %q", got)
	}
}
