package config

import "testing"

func TestOAuthIsOptInAndNeedsBotAndPublicURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"default off", Config{BotToken: "t", PublicBaseURL: "https://bridge.example"}, false},
		{"on with bot and url", Config{BotToken: "t", PublicBaseURL: "https://bridge.example", OAuthMode: "on"}, true},
		{"on without bot", Config{PublicBaseURL: "https://bridge.example", OAuthMode: "on"}, false},
		{"on without public url", Config{BotToken: "t", OAuthMode: "on"}, false},
		{"explicit off", Config{BotToken: "t", PublicBaseURL: "https://bridge.example", OAuthMode: "off"}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.HasOAuth(); got != tc.want {
			t.Errorf("%s: HasOAuth() = %t, want %t", tc.name, got, tc.want)
		}
		if tc.want && !tc.cfg.HasMCP() {
			t.Errorf("%s: OAuth must enable MCP without a static token", tc.name)
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
