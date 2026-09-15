package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Media         MediaConfig
	Addr          string
	DBPath        string
	PublicBaseURL string
	// SessionKey encrypts every stored Telegram user session (32 bytes).
	SessionKey []byte
	// OAuthMode "on" enables OAuth for MCP clients; it is off by default.
	OAuthMode              string
	OAuthExtraRedirectURIs []string

	BotToken string

	TelegramAPIID   int
	TelegramAPIHash string

	// Single-account deployments are imported once into the per-user model:
	// the session file becomes that account's encrypted session, and the
	// static tokens and group allowlist become that account's own.
	SessionPath         string
	MCPToken            string
	NotificationToken   string
	NotificationChatIDs []int64
}

func Load() (Config, error) {
	loadDotenvFiles(".env.local", ".env")

	cfg := Config{
		Addr:              envFirst("TELEGRAM_BRIDGE_ADDR", "ADDR"),
		DBPath:            envFirstDefault("TELEGRAM_BRIDGE_DB", "/data/telegram-bridge.db", "DB_PATH", "data/telegram-bridge.db"),
		SessionPath:       envFirstDefault("TELEGRAM_BRIDGE_SESSION", "/data/telegram.session", "TG_SESSION_PATH", "data/telegram.session"),
		PublicBaseURL:     strings.TrimRight(envFirst("TELEGRAM_BRIDGE_PUBLIC_URL", "PUBLIC_BASE_URL"), "/"),
		MCPToken:          envFirst("TELEGRAM_BRIDGE_MCP_TOKEN", "MCP_TOKEN"),
		NotificationToken: envFirst("TELEGRAM_BRIDGE_NOTIFICATION_TOKEN"),
		OAuthMode:         strings.ToLower(strings.TrimSpace(envFirst("TELEGRAM_BRIDGE_OAUTH"))),
		BotToken:          envFirst("TELEGRAM_BOT_TOKEN", "BOT_TOKEN"),
		TelegramAPIHash: envFirst(
			"TELEGRAM_API_HASH",
			"TG_API_HASH",
		),
	}

	if cfg.Addr == "" {
		if port := envFirst("PORT"); port != "" {
			cfg.Addr = ":" + port
		} else {
			cfg.Addr = ":8080"
		}
	}

	apiID, err := envInt("TELEGRAM_API_ID", "TG_API_ID")
	if err != nil {
		return Config{}, err
	}
	cfg.TelegramAPIID = apiID

	cfg.SessionKey, err = parseSessionKey(envFirst("TELEGRAM_BRIDGE_SESSION_KEY"))
	if err != nil {
		return Config{}, err
	}
	notificationChatIDs, err := parseInt64List(envFirst("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS")
	}
	for _, id := range notificationChatIDs {
		if id >= 0 || id < -1997852516352 || id == -1000000000000 {
			return Config{}, fmt.Errorf("TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS must contain only Bot API group IDs")
		}
	}
	cfg.NotificationChatIDs = notificationChatIDs
	switch cfg.OAuthMode {
	case "", "on", "off":
	default:
		return Config{}, fmt.Errorf("TELEGRAM_BRIDGE_OAUTH must be on or off")
	}
	cfg.OAuthExtraRedirectURIs = strings.FieldsFunc(envFirst("TELEGRAM_BRIDGE_OAUTH_REDIRECT_URIS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n'
	})
	if err := cfg.ValidateNotifications(); err != nil {
		return Config{}, err
	}
	cfg.Media, err = loadMediaConfig(cfg.DBPath)
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func loadDotenvFiles(paths ...string) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, rawLine := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(rawLine)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if _, exists := os.LookupEnv(key); exists {
				continue
			}
			os.Setenv(key, trimEnvValue(value))
		}
	}
}

func trimEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Addr, "addr", c.Addr, "HTTP listen address")
	fs.StringVar(&c.DBPath, "db", c.DBPath, "SQLite database path")
	fs.StringVar(&c.SessionPath, "session", c.SessionPath, "gotd Telegram session file path")
	fs.StringVar(&c.PublicBaseURL, "public-url", c.PublicBaseURL, "public HTTPS base URL for Telegram Mini App")
	fs.StringVar(&c.BotToken, "bot-token", c.BotToken, "Telegram bot token")
	fs.IntVar(&c.TelegramAPIID, "tg-api-id", c.TelegramAPIID, "Telegram API ID for gotd user sessions")
	fs.StringVar(&c.TelegramAPIHash, "tg-api-hash", c.TelegramAPIHash, "Telegram API hash for gotd user sessions")
}

func (c Config) HasBot() bool {
	return strings.TrimSpace(c.BotToken) != ""
}

func (c Config) HasTelegramUserAPI() bool {
	return c.TelegramAPIID != 0 && strings.TrimSpace(c.TelegramAPIHash) != ""
}

// HasOAuth reports whether MCP clients may authorize through the bot. OAuth
// is opt-in and needs the bot for approvals and a public URL for redirects.
func (c Config) HasOAuth() bool {
	return c.OAuthMode == "on" && c.HasBot() && c.PublicBaseURL != ""
}

// parseSessionKey accepts 32 bytes encoded as base64 or hex, for example
// the output of `openssl rand -base64 32`.
func parseSessionKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if key, err := decode(value); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, errors.New("TELEGRAM_BRIDGE_SESSION_KEY must be 32 random bytes in base64 or hex (openssl rand -base64 32)")
}

// ValidateSessionKey reports whether Telegram user sessions can be stored.
func (c Config) ValidateSessionKey() error {
	if len(c.SessionKey) != 32 {
		return errors.New("TELEGRAM_BRIDGE_SESSION_KEY is required to store Telegram sessions: generate one with openssl rand -base64 32")
	}
	return nil
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
	}
	return ""
}

func envFirstDefault(primary string, primaryDefault string, fallback string, fallbackDefault string) string {
	if value, ok := os.LookupEnv(primary); ok {
		return value
	}
	if value, ok := os.LookupEnv(fallback); ok {
		return value
	}
	if strings.HasPrefix(primaryDefault, "/data/") {
		if _, ok := os.LookupEnv("FLY_APP_NAME"); ok {
			return primaryDefault
		}
	}
	return fallbackDefault
}

func envInt(names ...string) (int, error) {
	for _, name := range names {
		value, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", name, err)
		}
		return parsed, nil
	}
	return 0, nil
}

func parseInt64List(value string) ([]int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';'
	})
	out := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid chat id %q: %w", part, err)
		}
		out = append(out, parsed)
	}
	return out, nil
}

func (c Config) ValidateNotifications() error {
	if c.NotificationToken == "" {
		return nil
	}
	if len(c.NotificationToken) < 32 || strings.TrimSpace(c.NotificationToken) != c.NotificationToken {
		return fmt.Errorf("TELEGRAM_BRIDGE_NOTIFICATION_TOKEN must be a dedicated random token of at least 32 characters without surrounding whitespace")
	}
	if c.NotificationToken == c.MCPToken {
		return fmt.Errorf("notification token must differ from the MCP token")
	}
	return nil
}
