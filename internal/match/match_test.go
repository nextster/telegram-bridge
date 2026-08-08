package match

import (
	"testing"

	"github.com/nextster/tg-radar/internal/db"
)

func TestEvaluateFlexibleRule(t *testing.T) {
	rules := []db.Keyword{{
		ID:           1,
		Phrase:       "repair stand",
		Enabled:      true,
		AnyTerms:     []string{"ремонтная стойка", "workstand", "prepstand"},
		AllTerms:     []string{"велосипед"},
		ExcludeTerms: []string{"продано", "sold"},
		Sources:      []db.RuleSource{{PeerType: "channel", PeerID: 42}},
	}}

	got := Evaluate("Велосипед: отличный PREPSTAND, отправлю", "channel", 42, rules)
	if len(got) != 1 {
		t.Fatalf("matches = %d, want 1", len(got))
	}
	if got[0].Reason != "any: prepstand; all: велосипед" {
		t.Fatalf("reason = %q", got[0].Reason)
	}
	if got[0].Score != 115 {
		t.Fatalf("score = %d, want 115", got[0].Score)
	}
	if got := Evaluate("Велосипед prepstand — продано", "channel", 42, rules); len(got) != 0 {
		t.Fatalf("excluded matches = %d, want 0", len(got))
	}
	if got := Evaluate("Велосипед prepstand", "channel", 99, rules); len(got) != 0 {
		t.Fatalf("wrong-source matches = %d, want 0", len(got))
	}
}

func TestEvaluateFuzzyPhrase(t *testing.T) {
	rules := []db.Keyword{{Phrase: "torque wrench", Enabled: true, AnyTerms: []string{"динамометрический ключ"}}}
	got := Evaluate("Продам динамометрическй ключ", "channel", 1, rules)
	if len(got) != 1 || got[0].Reason != "fuzzy: динамометрический ключ" {
		t.Fatalf("got %#v", got)
	}
}

func TestEvaluateRequiredAnyGroupsAndPreferences(t *testing.T) {
	rules := []db.Keyword{{
		Phrase:            "front light",
		Enabled:           true,
		AnyTerms:          []string{"front bike light", "велофара"},
		RequiredAnyGroups: [][]string{{"usb c", "type c"}, {"1000lm", "1200lm", "1600lm"}},
		PreferredTerms:    []string{"daytime flash", "garmin mount"},
		ExcludeTerms:      []string{"sold"},
	}}
	got := Evaluate("Front bike light 1200lm, USB-C, daytime flash and Garmin mount", "channel", 1, rules)
	if len(got) != 1 {
		t.Fatalf("matches = %d, want 1", len(got))
	}
	if got[0].Reason != "any: front bike light; required: usb c, 1200lm; preferred: daytime flash, garmin mount" {
		t.Fatalf("reason = %q", got[0].Reason)
	}
	if got := Evaluate("Front bike light 800lm, USB-C", "channel", 1, rules); len(got) != 0 {
		t.Fatalf("underpowered matches = %d, want 0", len(got))
	}
	if got := Evaluate("Front bike light 1200lm, Micro-USB", "channel", 1, rules); len(got) != 0 {
		t.Fatalf("wrong connector matches = %d, want 0", len(got))
	}
}

func TestEvaluateUsesTokenSafeTerms(t *testing.T) {
	rules := []db.Keyword{{
		Phrase:   "numbers",
		Enabled:  true,
		AnyTerms: []string{"12", "36", "sold"},
	}}
	for _, text := range []string{"1200 lumens", "136 mm", "unsold cassette"} {
		if got := Evaluate(text, "channel", 1, rules); len(got) != 0 {
			t.Fatalf("%q matched %#v", text, got)
		}
	}
	if got := Evaluate("12 speed cassette 10-36", "channel", 1, rules); len(got) != 1 {
		t.Fatalf("whole tokens matches = %d, want 1", len(got))
	}
}

func TestEvaluateScopedRuleFailsClosedWithoutSource(t *testing.T) {
	rules := []db.Keyword{{
		Phrase:   "scoped",
		Enabled:  true,
		AnyTerms: []string{"radar"},
		Sources:  []db.RuleSource{{PeerType: "channel", PeerID: 42}},
	}}
	if got := Evaluate("radar", "", 0, rules); len(got) != 0 {
		t.Fatalf("missing source matches = %d, want 0", len(got))
	}
}

func TestEvaluateNumericMinimumRequiresAdjacentUnit(t *testing.T) {
	rules := []db.Keyword{{
		Phrase:            "light",
		Enabled:           true,
		AnyTerms:          []string{"front bike light"},
		RequiredAnyGroups: [][]string{{"num>=1000:lm|lumen|lumens|лм|люмен|люменов"}},
	}}
	for _, text := range []string{
		"Front bike light 1200lm",
		"Front bike light 1050 lumens",
		"Front bike light 1600 люменов",
	} {
		if got := Evaluate(text, "channel", 1, rules); len(got) != 1 {
			t.Fatalf("%q matches = %d, want 1", text, len(got))
		}
	}
	for _, text := range []string{
		"Front bike light 800lm, price 1200 GEL",
		"Front bike light 800lm, battery 5000mAh",
		"Front bike light, price 1600",
	} {
		if got := Evaluate(text, "channel", 1, rules); len(got) != 0 {
			t.Fatalf("%q matches = %d, want 0", text, len(got))
		}
	}
}

func TestEvaluateExclusionsAreExactOnly(t *testing.T) {
	rules := []db.Keyword{{
		Phrase:       "light",
		Enabled:      true,
		AnyTerms:     []string{"велофара"},
		ExcludeTerms: []string{"продан"},
	}}
	if got := Evaluate("Продам велофару", "channel", 1, rules); len(got) != 1 {
		t.Fatalf("sale listing matches = %d, want 1", len(got))
	}
	if got := Evaluate("Велофара продана", "channel", 1, rules); len(got) != 1 {
		t.Fatalf("different exact inflection matches = %d, want 1", len(got))
	}
	if got := Evaluate("Велофара продан", "channel", 1, rules); len(got) != 0 {
		t.Fatalf("exact exclusion matches = %d, want 0", len(got))
	}
}

func TestLooksLikeCompleteBike(t *testing.T) {
	complete := []string{
		"#bikes #road Trek Domane with SRAM Rival",
		"#\u2060bikes #gravel Cannondale Topstone",
		"Продаю велосипед Giant Defy, размер M",
		"Selling a gravel bike. Carbon frame, DT Swiss wheels, hydraulic brakes, SRAM cassette and compact handlebar.",
		"Specification: frame carbon, fork carbon, wheels alloy, Shimano cassette, hydraulic brakes, handlebar 38cm.",
	}
	for _, text := range complete {
		if !LooksLikeCompleteBike(text) {
			t.Fatalf("%q was not classified as a complete bike", text)
		}
	}

	standalone := []string{
		"#bikeshop SRAM XG-1250 cassette",
		"SRAM XG-1250 cassette compatible with road bikes",
		"Front bike light for frame or handlebar mounting",
		"Floor-to-ceiling rack for two bikes with padded hooks",
		"Complete groupset: cassette, chain, cranks, derailleurs and shifters",
	}
	for _, text := range standalone {
		if LooksLikeCompleteBike(text) {
			t.Fatalf("%q was misclassified as a complete bike", text)
		}
	}
}

func TestEvaluateCanSuppressCompleteBikeListings(t *testing.T) {
	rule := db.Keyword{
		Phrase:              "cassette",
		Enabled:             true,
		AnyTerms:            []string{"xg-1250"},
		ExcludeCompleteBike: true,
	}
	complete := "#bikes #road Trek. SRAM XG-1250 cassette 10-36."
	if got := Evaluate(complete, "channel", 1, []db.Keyword{rule}); len(got) != 0 {
		t.Fatalf("complete-bike listing matched %#v", got)
	}
	rule.ExcludeCompleteBike = false
	if got := Evaluate(complete, "channel", 1, []db.Keyword{rule}); len(got) != 1 {
		t.Fatalf("unfiltered rule matches = %d, want 1", len(got))
	}
}
