package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type MediaConfig struct {
	Enabled           bool
	APIKey            string
	Directory         string
	AudioModel        string
	ImageModel        string
	CacheRevision     string
	MaxBytes          int64
	MaxSeconds        int
	MaxDiskBytes      int64
	RetentionHours    int
	DailyBudgetMicros int64
	TotalBudgetMicros int64
}

func DefaultMediaConfig(dbPath string) MediaConfig {
	return MediaConfig{
		Directory:  filepath.Join(filepath.Dir(dbPath), "media"),
		AudioModel: "openai/gpt-transcribe", ImageModel: "openai/gpt-4.1-mini", CacheRevision: "1",
		MaxBytes: 20 << 20, MaxSeconds: 600, MaxDiskBytes: 200 << 20, RetentionHours: 24,
		DailyBudgetMicros: 1_000_000, TotalBudgetMicros: 5_000_000,
	}
}

func loadMediaConfig(dbPath string) (MediaConfig, error) {
	c := DefaultMediaConfig(dbPath)
	c.APIKey = os.Getenv("OPENROUTER_API_KEY")
	if value := os.Getenv("TELEGRAM_BRIDGE_MEDIA_ENABLED"); value != "" {
		var err error
		c.Enabled, err = strconv.ParseBool(value)
		if err != nil {
			return c, fmt.Errorf("TELEGRAM_BRIDGE_MEDIA_ENABLED must be a boolean")
		}
	}
	for name, target := range map[string]*string{
		"TELEGRAM_BRIDGE_MEDIA_DIR":            &c.Directory,
		"TELEGRAM_BRIDGE_AUDIO_MODEL":          &c.AudioModel,
		"TELEGRAM_BRIDGE_IMAGE_MODEL":          &c.ImageModel,
		"TELEGRAM_BRIDGE_MEDIA_CACHE_REVISION": &c.CacheRevision,
	} {
		if value := os.Getenv(name); value != "" {
			*target = value
		}
	}
	for name, target := range map[string]*int64{
		"TELEGRAM_BRIDGE_MEDIA_MAX_BYTES":             &c.MaxBytes,
		"TELEGRAM_BRIDGE_MEDIA_MAX_DISK_BYTES":        &c.MaxDiskBytes,
		"TELEGRAM_BRIDGE_MEDIA_DAILY_BUDGET_MICROUSD": &c.DailyBudgetMicros,
		"TELEGRAM_BRIDGE_MEDIA_TOTAL_BUDGET_MICROUSD": &c.TotalBudgetMicros,
	} {
		if value := os.Getenv(name); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return c, fmt.Errorf("%s must be an integer", name)
			}
			*target = n
		}
	}
	for name, target := range map[string]*int{
		"TELEGRAM_BRIDGE_MEDIA_MAX_SECONDS":     &c.MaxSeconds,
		"TELEGRAM_BRIDGE_MEDIA_RETENTION_HOURS": &c.RetentionHours,
	} {
		if value := os.Getenv(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return c, fmt.Errorf("%s must be an integer", name)
			}
			*target = n
		}
	}
	return c, c.Validate()
}

func (c MediaConfig) Validate() error {
	if c.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("media processing requires OPENROUTER_API_KEY")
	}
	if c.AudioModel != "openai/gpt-transcribe" {
		return fmt.Errorf("unsupported audio model; supported: openai/gpt-transcribe")
	}
	if c.ImageModel != "openai/gpt-4.1-mini" {
		return fmt.Errorf("unsupported image model; supported: openai/gpt-4.1-mini")
	}
	if c.Directory == "" || len(c.CacheRevision) == 0 || len(c.CacheRevision) > 64 {
		return fmt.Errorf("media directory and bounded cache revision are required")
	}
	if c.MaxBytes < 1 || c.MaxBytes > 24_000_000 || c.MaxSeconds < 1 || c.MaxSeconds > 600 {
		return fmt.Errorf("media limits must be 1..24000000 bytes and 1..600 seconds")
	}
	if c.MaxDiskBytes < c.MaxBytes*2 || c.MaxDiskBytes > 1<<30 || c.RetentionHours < 1 || c.RetentionHours > 168 {
		return fmt.Errorf("invalid media disk or retention limit")
	}
	if c.DailyBudgetMicros < 1 || c.TotalBudgetMicros < c.DailyBudgetMicros || c.TotalBudgetMicros > 100_000_000 {
		return fmt.Errorf("invalid media budgets; total must be >= daily and <= 100 USD")
	}
	return nil
}
