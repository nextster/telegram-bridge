package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr          string
	DBPath        string
	SessionPath   string
	PublicBaseURL string
	MCPToken      string

	BotToken        string
	BotAdminChatIDs []int64

	TelegramAPIID    int
	TelegramAPIHash  string
	TelegramPhone    string
	TelegramPassword string
}

func Load() (Config, error) {
	loadDotenvFiles(".env.local", ".env")

	cfg := Config{
		Addr:          envFirst("TG_RADAR_ADDR", "ADDR"),
		DBPath:        envFirstDefault("TG_RADAR_DB", "/data/tg-radar.db", "DB_PATH", "data/tg-radar.db"),
		SessionPath:   envFirstDefault("TG_RADAR_SESSION", "/data/telegram.session", "TG_SESSION_PATH", "data/telegram.session"),
		PublicBaseURL: strings.TrimRight(envFirst("TG_RADAR_PUBLIC_URL", "PUBLIC_BASE_URL"), "/"),
		MCPToken:      envFirst("TG_RADAR_MCP_TOKEN", "MCP_TOKEN"),
		BotToken:      envFirst("TELEGRAM_BOT_TOKEN", "BOT_TOKEN"),
		TelegramAPIHash: envFirst(
			"TELEGRAM_API_HASH",
			"TG_API_HASH",
		),
		TelegramPhone:    envFirst("TELEGRAM_PHONE", "TG_PHONE"),
		TelegramPassword: envFirst("TELEGRAM_PASSWORD", "TG_PASSWORD"),
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

	adminChatIDs, err := parseInt64List(envFirst("TG_RADAR_ADMIN_CHAT_IDS", "ADMIN_CHAT_IDS"))
	if err != nil {
		return Config{}, err
	}
	cfg.BotAdminChatIDs = adminChatIDs

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
	fs.StringVar(&c.MCPToken, "mcp-token", c.MCPToken, "bearer token protecting the MCP endpoint")
	fs.StringVar(&c.BotToken, "bot-token", c.BotToken, "Telegram bot token")
	fs.Func("admin-chat-ids", "comma-separated Telegram chat ids allowed to run bot admin commands", func(value string) error {
		ids, err := parseInt64List(value)
		if err != nil {
			return err
		}
		c.BotAdminChatIDs = ids
		return nil
	})
	fs.IntVar(&c.TelegramAPIID, "tg-api-id", c.TelegramAPIID, "Telegram API ID for gotd user session")
	fs.StringVar(&c.TelegramAPIHash, "tg-api-hash", c.TelegramAPIHash, "Telegram API hash for gotd user session")
	fs.StringVar(&c.TelegramPhone, "tg-phone", c.TelegramPhone, "Telegram phone number for login")
	fs.StringVar(&c.TelegramPassword, "tg-password", c.TelegramPassword, "Telegram 2FA password for login")
}

func (c Config) HasBot() bool {
	return strings.TrimSpace(c.BotToken) != ""
}

func (c Config) IsConfiguredBotAdmin(chatID int64) bool {
	for _, id := range c.BotAdminChatIDs {
		if id == chatID {
			return true
		}
	}
	return false
}

func (c Config) HasTelegramUserAPI() bool {
	return c.TelegramAPIID != 0 && strings.TrimSpace(c.TelegramAPIHash) != ""
}

func (c Config) HasMCP() bool {
	return strings.TrimSpace(c.MCPToken) != ""
}

func (c Config) ValidateLogin() error {
	if c.TelegramAPIID == 0 {
		return fmt.Errorf("telegram API ID is required: set TELEGRAM_API_ID or pass -tg-api-id")
	}
	if strings.TrimSpace(c.TelegramAPIHash) == "" {
		return fmt.Errorf("telegram API hash is required: set TELEGRAM_API_HASH or pass -tg-api-hash")
	}
	if strings.TrimSpace(c.TelegramPhone) == "" {
		return fmt.Errorf("telegram phone is required: set TELEGRAM_PHONE or pass -tg-phone")
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
			return nil, fmt.Errorf("TG_RADAR_ADMIN_CHAT_IDS contains invalid chat id %q: %w", part, err)
		}
		out = append(out, parsed)
	}
	return out, nil
}
