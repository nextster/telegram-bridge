package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/match"
)

// Personal rule packs stay outside git (rules/*.json is ignored); this test
// keeps the published example importable and matching.
func TestExampleRulesImportAndMatch(t *testing.T) {
	payload, err := os.Open(filepath.Join("..", "..", "rules", "example.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()

	ctx := context.Background()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "rules.db")}
	seed, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.AddKeyword(ctx, 7, "bike light"); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	var output bytes.Buffer
	if err := importRules(ctx, cfg, 7, payload, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "Imported 2 watch rules; deleted 1 old rules") {
		t.Fatalf("output = %q", got)
	}

	store, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := store.ListKeywords(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	matched := func(text string, peerID int64, name string) bool {
		for _, result := range match.Evaluate(text, "channel", peerID, rules) {
			if result.Keyword.Phrase == name {
				return true
			}
		}
		return false
	}
	const light, lamp = "bike · front light · 1000+ lm", "home · desk lamp"
	if !matched("Велофара 1200 лм, зарядка Type-C", 42, light) {
		t.Fatal("front light listing did not match")
	}
	if matched("Велофара 1200 лм, зарядка Type-C, продано", 42, light) {
		t.Fatal("default exclusion did not suppress a sold listing")
	}
	if matched("Велофара 800 лм, Type-C", 42, light) {
		t.Fatal("numeric minimum accepted 800 lm")
	}
	if !matched("Настольная лампа, как новая", 1000000001, lamp) || matched("Настольная лампа, как новая", 42, lamp) {
		t.Fatal("source-scoped rule matched the wrong channel")
	}
}
