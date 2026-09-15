package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestLegacyNotificationAllowlistConfiguration(t *testing.T) {
	t.Setenv("TELEGRAM_API_ID", "")
	for _, value := range []string{"", "-1001234567890", "-123,-1001234567890"} {
		t.Setenv("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS", value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("valid allowlist: %v", err)
		}
		if value == "" && len(cfg.NotificationChatIDs) != 0 {
			t.Fatal("default must not allow any group")
		}
	}
	for _, value := range []string{"1234567890", "0", "-1000000000000", "-1997852516353", "bad"} {
		t.Setenv("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS", value)
		if _, err := Load(); err == nil {
			t.Fatalf("accepted invalid allowlist %q", value)
		}
	}
}

func TestLegacyNotificationTokenMustBeIndependent(t *testing.T) {
	token := strings.Repeat("n", 32)
	for _, cfg := range []Config{
		{NotificationToken: "short"},
		{NotificationToken: token + " "},
		{NotificationToken: token, MCPToken: token},
	} {
		if cfg.ValidateNotifications() == nil {
			t.Fatal("accepted invalid or reused notification credential")
		}
	}
	if (Config{NotificationToken: token, MCPToken: "read"}).ValidateNotifications() != nil {
		t.Fatal("rejected a dedicated notification credential")
	}
}

func TestSessionKey(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(key),
		base64.RawURLEncoding.EncodeToString(key),
		hex.EncodeToString(key),
		" " + base64.StdEncoding.EncodeToString(key) + "\n",
	} {
		got, err := parseSessionKey(encoded)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("parseSessionKey(%q) = %x, %v", encoded, got, err)
		}
	}
	for _, encoded := range []string{"short", base64.StdEncoding.EncodeToString(key[:16]), hex.EncodeToString(append(key, 1))} {
		if _, err := parseSessionKey(encoded); err == nil {
			t.Fatalf("accepted session key %q", encoded)
		}
	}
	if (Config{}).ValidateSessionKey() == nil {
		t.Fatal("missing session key accepted")
	}
	if (Config{SessionKey: key}).ValidateSessionKey() != nil {
		t.Fatal("valid session key rejected")
	}
}
