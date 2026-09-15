package config

import "testing"

func TestOAuthIsOptInAndNeedsBotPublicURLAndTelegramLogin(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"default off", Config{BotToken: "123:t", PublicBaseURL: "https://bridge.example", TelegramLoginSecret: "s"}, false},
		{"on with bot, url and login secret", Config{BotToken: "123:t", PublicBaseURL: "https://bridge.example", OAuthMode: "on", TelegramLoginSecret: "s"}, true},
		{"on without login secret", Config{BotToken: "123:t", PublicBaseURL: "https://bridge.example", OAuthMode: "on"}, false},
		{"on with a malformed bot token", Config{BotToken: "t", PublicBaseURL: "https://bridge.example", OAuthMode: "on", TelegramLoginSecret: "s"}, false},
		{"on without bot", Config{PublicBaseURL: "https://bridge.example", OAuthMode: "on", TelegramLoginSecret: "s"}, false},
		{"on without public url", Config{BotToken: "123:t", OAuthMode: "on", TelegramLoginSecret: "s"}, false},
		{"explicit off", Config{BotToken: "123:t", PublicBaseURL: "https://bridge.example", OAuthMode: "off", TelegramLoginSecret: "s"}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.HasOAuth(); got != tc.want {
			t.Errorf("%s: HasOAuth() = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestOAuthModeValidation(t *testing.T) {
	t.Setenv("TELEGRAM_API_ID", "")
	t.Setenv("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS", "")
	for _, value := range []string{"maybe", "auto"} {
		t.Setenv("TELEGRAM_BRIDGE_OAUTH", value)
		if _, err := Load(); err == nil {
			t.Fatalf("accepted TELEGRAM_BRIDGE_OAUTH=%q", value)
		}
	}
	t.Setenv("TELEGRAM_BRIDGE_OAUTH", "ON")
	if cfg, err := Load(); err != nil || cfg.OAuthMode != "on" {
		t.Fatalf("mode=%q err=%v", cfg.OAuthMode, err)
	}
}
