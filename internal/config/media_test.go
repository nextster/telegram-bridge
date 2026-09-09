package config

import "testing"

func TestMediaConfigFailsClosed(t *testing.T) {
	c := DefaultMediaConfig("data/test.db")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Enabled = true
	if err := c.Validate(); err == nil {
		t.Fatal("processing enabled without key")
	}
	c.APIKey = "fake"
	for _, mutate := range []func(*MediaConfig){
		func(c *MediaConfig) { c.Concurrency = 0 }, func(c *MediaConfig) { c.Concurrency = 4 },
		func(c *MediaConfig) { c.MaxBytes = 25_000_000 }, func(c *MediaConfig) { c.MaxSeconds = 601 },
		func(c *MediaConfig) { c.RetentionHours = 0 }, func(c *MediaConfig) { c.DailyBudgetMicros = 0 },
		func(c *MediaConfig) { c.TotalBudgetMicros = 1 }, func(c *MediaConfig) { c.AudioModel = "unapproved/model" },
		func(c *MediaConfig) { c.ImageModel = "unapproved/model" },
	} {
		copy := c
		mutate(&copy)
		if err := copy.Validate(); err == nil {
			t.Fatal("invalid media configuration accepted")
		}
	}
}

func TestMediaConfigurationUsesOpenRouterSecret(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "secret-fixture")
	t.Setenv("TELEGRAM_BRIDGE_MEDIA_ENABLED", "true")
	t.Setenv("TELEGRAM_BRIDGE_MEDIA_CONCURRENCY", "2")
	c, err := loadMediaConfig("/data/test.db")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "secret-fixture" || c.Directory != "/data/media" || c.AudioModel != "openai/gpt-transcribe" || c.Concurrency != 2 {
		t.Fatal("wrong media configuration")
	}
	t.Setenv("TELEGRAM_BRIDGE_MEDIA_MAX_BYTES", "secret-not-an-integer")
	if _, err := loadMediaConfig("/data/test.db"); err == nil || err.Error() != "TELEGRAM_BRIDGE_MEDIA_MAX_BYTES must be an integer" {
		t.Fatal("invalid config not sanitized")
	}
}
