package config

import "testing"

func TestNotificationAllowlistConfiguration(t *testing.T) {
	t.Setenv("TELEGRAM_API_ID", "")
	t.Setenv("TELEGRAM_BRIDGE_ADMIN_CHAT_IDS", "")
	for _, value := range []string{"", "-1001234567890", "-123,-1001234567890"} {
		t.Setenv("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS", value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("valid allowlist: %v", err)
		}
		if value == "" && len(cfg.NotificationChatIDs) != 0 {
			t.Fatal("default must disable notifications")
		}
	}
	for _, value := range []string{"1234567890", "0", "-1000000000000", "-1997852516353", "bad"} {
		t.Setenv("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS", value)
		if _, err := Load(); err == nil {
			t.Fatalf("accepted invalid allowlist %q", value)
		}
	}
}
