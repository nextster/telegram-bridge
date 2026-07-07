package bot

import (
	"testing"

	"github.com/nextster/tg-radar/internal/db"
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
