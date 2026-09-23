package bot

import (
	"testing"

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
