package bot

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mymmrac/telego"

	"github.com/nextster/telegram-bridge/internal/config"
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

func TestPrivateAlertChatIDUsesOnlyLoggedInOwner(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/bot-admin.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, chatID := range []int64{111, 222} {
		if err := store.UpsertSubscriber(ctx, db.Subscriber{ChatID: chatID}); err != nil {
			t.Fatal(err)
		}
	}

	service := &Service{store: store, cfg: config.Config{BotAdminChatIDs: []int64{999}}}
	if got, ok, err := service.privateAlertChatID(ctx, 222); err != nil || !ok || got != 222 {
		t.Fatalf("subscribed owner: id=%d ok=%v err=%v", got, ok, err)
	}
	if got, ok, err := service.privateAlertChatID(ctx, 999); err != nil || ok || got != 0 {
		t.Fatalf("unsubscribed configured admin: id=%d ok=%v err=%v", got, ok, err)
	}
}

func TestAdminCommandAndSubscriptionBoundaries(t *testing.T) {
	for _, command := range []string{"add", "watch", "del", "delete", "keywords", "recent", "login", "loginstatus", "cancel"} {
		if !adminOnlyCommand(command) {
			t.Fatalf("%q should require an admin chat", command)
		}
	}
	for _, command := range []string{"start", "stop", "help", "unknown"} {
		if adminOnlyCommand(command) {
			t.Fatalf("%q unexpectedly requires the admin-command gate", command)
		}
	}

	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/subscriptions.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	service := &Service{store: store}
	if allowed, err := service.canSubscribe(ctx, 111); err != nil || !allowed {
		t.Fatalf("first subscriber allowed=%v err=%v", allowed, err)
	}
	if err := store.UpsertSubscriber(ctx, db.Subscriber{ChatID: 111}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := service.canSubscribe(ctx, 222); err != nil || allowed {
		t.Fatalf("second subscriber allowed=%v err=%v", allowed, err)
	}
	if allowed, err := service.canSubscribe(ctx, 111); err != nil || !allowed {
		t.Fatalf("existing admin subscriber allowed=%v err=%v", allowed, err)
	}

	configured := &Service{store: store, cfg: config.Config{BotAdminChatIDs: []int64{222}}}
	if allowed, err := configured.canSubscribe(ctx, 111); err != nil || allowed {
		t.Fatalf("unconfigured chat allowed=%v err=%v", allowed, err)
	}
	if allowed, err := configured.canSubscribe(ctx, 222); err != nil || !allowed {
		t.Fatalf("configured admin allowed=%v err=%v", allowed, err)
	}
}

func TestOnlyAdminReceivesDashboardAndDestructiveButtons(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/admin-markup.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertSubscriber(ctx, db.Subscriber{ChatID: 111}); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, cfg: config.Config{PublicBaseURL: "https://telegram-bridge.example"}}

	if service.webAppMarkup(ctx, 111) == nil {
		t.Fatal("admin dashboard button is missing")
	}
	if service.webAppMarkup(ctx, 222) != nil {
		t.Fatal("non-admin received a dashboard button")
	}

	event := db.Event{ID: 9, RuleID: 4, Keyword: "private rule", SourcePeerType: "channel", SourcePeerID: 123, MessageID: 77}
	adminMarkup, ok := service.eventMarkup(ctx, event, 111).(*telego.InlineKeyboardMarkup)
	if !ok || len(adminMarkup.InlineKeyboard) != 2 {
		t.Fatalf("admin event markup = %#v, want link and stop rows", adminMarkup)
	}
	actions := adminMarkup.InlineKeyboard[1]
	if len(actions) != 2 || actions[0].Text != "Правило" || actions[0].WebApp == nil || actions[0].WebApp.URL != "https://telegram-bridge.example/#rule-4" {
		t.Fatalf("admin event actions = %#v, want rule deep-link and stop button", actions)
	}
	nonAdminMarkup, ok := service.eventMarkup(ctx, event, 222).(*telego.InlineKeyboardMarkup)
	if !ok || len(nonAdminMarkup.InlineKeyboard) != 1 {
		t.Fatalf("non-admin event markup = %#v, want link row only", nonAdminMarkup)
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

func TestFormatCodexSnapshotPreservesUsefulMarkdown(t *testing.T) {
	got := formatCodexSnapshot(db.CodexThreadSnapshot{
		CWD: "/path/to/telegram-bridge", Status: "active",
		MessageRole: "assistant",
		Message:     "**Done** with [details](https://example.com).\n\n- one\n- `two`\n\n```go\nfmt.Println(\"ok\")\n```",
	})
	for _, want := range []string{
		"⏳ working · <code>telegram-bridge</code>",
		"<b>Done</b>",
		"<a href=\"https://example.com\">details</a>",
		"• one",
		"<code>two</code>",
		"<pre><code class=\"language-go\">fmt.Println(&#34;ok&#34;)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted snapshot does not contain %q: %s", want, got)
		}
	}
}

func TestFormatCodexSnapshotHighlightsWaitingForUser(t *testing.T) {
	got := formatCodexSnapshot(db.CodexThreadSnapshot{
		CWD: "/tmp/project", Status: "active", ActiveFlags: []string{"waitingOnUserInput"},
		MessageRole: "user", Message: "Which one?",
	})
	if got != "🙋 waiting for you · <code>project</code> · you\n\nWhich one?" {
		t.Fatalf("formatCodexSnapshot() = %q", got)
	}
}

func TestResolveGeneralCodexProject(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, t.TempDir()+"/general-codex.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, project := range []db.CodexProject{
		{Slug: "_active", Title: "Active", TelegramChannelID: 100, TelegramChatID: -1000000000100},
		{Slug: "telegram-bridge", Title: "telegram-bridge", TelegramChannelID: 200, TelegramChatID: -1000000000200},
	} {
		if err := store.UpsertCodexProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{store: store}
	active, _, err := store.GetCodexProject(ctx, "_active")
	if err != nil {
		t.Fatal(err)
	}
	project, prompt, err := service.resolveGeneralCodexProject(ctx, active, "make it nicer")
	if err != nil || project != "telegram-bridge" || prompt != "make it nicer" {
		t.Fatalf("single project resolution = %q, %q, %v", project, prompt, err)
	}
	project, prompt, err = service.resolveGeneralCodexProject(ctx, active, "telegram-bridge :: inspect this")
	if err != nil || project != "telegram-bridge" || prompt != "inspect this" {
		t.Fatalf("explicit project resolution = %q, %q, %v", project, prompt, err)
	}
}
