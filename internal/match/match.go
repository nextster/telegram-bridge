package match

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/nextster/tg-radar/internal/db"
)

type Result struct {
	Keyword db.Keyword
	Reason  string
	Score   int
}

// Find preserves the original single-phrase matcher API for callers and tests.
func Find(text string, keywords []db.Keyword) []db.Keyword {
	results := Evaluate(text, "", 0, keywords)
	out := make([]db.Keyword, 0, len(results))
	for _, result := range results {
		out = append(out, result.Keyword)
	}
	return out
}

func Evaluate(text, peerType string, peerID int64, keywords []db.Keyword) []Result {
	normalizedText := normalize(text)
	if normalizedText == "" {
		return nil
	}
	completeBike := LooksLikeCompleteBike(text)
	var out []Result
	for _, keyword := range keywords {
		if !keyword.Enabled || !sourceAllowed(keyword.Sources, peerType, peerID) {
			continue
		}
		if keyword.ExcludeCompleteBike && completeBike {
			continue
		}
		anyTerms := keyword.AnyTerms
		if len(anyTerms)+len(keyword.AllTerms)+len(keyword.RequiredAnyGroups) == 0 {
			anyTerms = []string{keyword.Phrase}
		}
		if matched, _ := firstExactMatch(normalizedText, keyword.ExcludeTerms); matched {
			continue
		}

		allMatched := make([]string, 0, len(keyword.AllTerms))
		score := 0
		failed := false
		for _, term := range keyword.AllTerms {
			matched, fuzzy := termMatches(normalizedText, term)
			if !matched {
				failed = true
				break
			}
			allMatched = append(allMatched, term)
			if fuzzy {
				score += 8
			} else {
				score += 10
			}
		}
		if failed {
			continue
		}

		requiredMatched := make([]string, 0, len(keyword.RequiredAnyGroups))
		for _, group := range keyword.RequiredAnyGroups {
			matched, term, fuzzy := firstMatch(normalizedText, group)
			if !matched {
				failed = true
				break
			}
			requiredMatched = append(requiredMatched, term)
			if fuzzy {
				score += 8
			} else {
				score += 10
			}
		}
		if failed {
			continue
		}

		anyMatched, anyTerm, anyFuzzy := firstMatch(normalizedText, anyTerms)
		if len(anyTerms) > 0 && !anyMatched {
			continue
		}
		parts := make([]string, 0, 4)
		if anyMatched {
			kind := "any"
			if anyFuzzy {
				kind = "fuzzy"
				score += 80
			} else {
				score += 100
			}
			parts = append(parts, fmt.Sprintf("%s: %s", kind, anyTerm))
		}
		if len(allMatched) > 0 {
			parts = append(parts, "all: "+strings.Join(allMatched, ", "))
		}
		if len(requiredMatched) > 0 {
			parts = append(parts, "required: "+strings.Join(requiredMatched, ", "))
		}
		preferredMatched := make([]string, 0, len(keyword.PreferredTerms))
		for _, term := range keyword.PreferredTerms {
			matched, fuzzy := termMatches(normalizedText, term)
			if !matched {
				continue
			}
			preferredMatched = append(preferredMatched, strings.TrimSpace(term))
			if fuzzy {
				score++
			} else {
				score += 2
			}
		}
		if len(preferredMatched) > 0 {
			parts = append(parts, "preferred: "+strings.Join(preferredMatched, ", "))
		}
		if len(keyword.Sources) > 0 {
			score += 5
		}
		out = append(out, Result{Keyword: keyword, Reason: strings.Join(parts, "; "), Score: score})
	}
	return out
}

// LooksLikeCompleteBike identifies listings for an assembled bicycle, rather
// than standalone parts or accessories. It deliberately uses only strong
// marketplace signals: Cycling Market's #bikes taxonomy, explicit whole-bike
// sale phrases, or a component-rich specification sheet.
func LooksLikeCompleteBike(text string) bool {
	if containsHashtag(text, "bikes") {
		return true
	}

	normalized := normalize(text)
	if containsAnyExact(normalized, []string{
		"велосипед в сборе",
		"велосипед целиком",
		"полный велосипед",
		"продаю велосипед",
		"продам велосипед",
		"продается велосипед",
		"complete bike",
		"complete bicycle",
		"full bike",
		"bike for sale",
		"bicycle for sale",
		"selling my bike",
		"selling this bike",
		"იყიდება ველოსიპედი",
		"ველოსიპედი იყიდება",
		"ვყიდი ველოსიპედს",
		"სრული ველოსიპედი",
	}) {
		return true
	}

	families := [][]string{
		{
			"frame", "frameset", "fork", "рама", "вилка",
			"ჩარჩო", "ჩანგალი",
		},
		{
			"wheel", "wheels", "wheelset", "rim", "rims", "tyre", "tyres",
			"tire", "tires", "tubeless", "hub", "hubs", "колесо", "колеса",
			"обода", "обод", "покрышки", "покрышка", "втулки", "втулка",
			"ბორბალი", "ბორბლები", "საბურავი", "საბურავები",
		},
		{
			"cassette", "chain", "crank", "crankset", "derailleur", "derailleurs",
			"shifter", "shifters", "groupset", "chainring", "кассета", "цепь",
			"шатуны", "система", "переключатель", "переключатели", "манетки",
			"звезда", "ტრანსმისია", "კასეტა", "ჯაჭვი",
		},
		{
			"brake", "brakes", "caliper", "calipers", "rotor", "rotors",
			"hydraulic", "тормоз", "тормоза", "калипер", "калиперы", "ротор",
			"роторы", "მუხრუჭი", "მუხრუჭები",
		},
		{
			"handlebar", "handlebars", "stem", "headset", "bar tape", "руль",
			"вынос", "рулевая", "обмотка", "საჭე",
		},
		{
			"saddle", "seatpost", "pedal", "pedals", "седло", "подседельный",
			"подседельник", "педали", "უნაგირი", "პედლები",
		},
	}
	familyCount := 0
	hasFrameFamily := false
	for i, family := range families {
		if containsAnyExact(normalized, family) {
			familyCount++
			hasFrameFamily = hasFrameFamily || i == 0
		}
	}

	bikeIdentity := containsAnyExact(normalized, []string{
		"road bike", "gravel bike", "mountain bike", "mtb bike", "city bike",
		"шоссейный велосипед", "гравийный велосипед", "горный велосипед",
		"велосипед gravel", "велосипед road", "ველოსიპედი",
	})
	if bikeIdentity && familyCount >= 3 {
		return true
	}

	specSheet := containsAnyExact(normalized, []string{
		"комплектация", "характеристики", "specification", "specifications",
		"bike specs", "frame size", "размер рамы", "bike size", "ზომა",
	})
	if specSheet && hasFrameFamily && familyCount >= 4 {
		return true
	}

	saleSignal := containsAnyExact(normalized, []string{
		"продам", "продаю", "продается", "selling", "for sale", "იყიდება", "ვყიდი",
	})
	return saleSignal && hasFrameFamily && familyCount >= 5
}

func containsHashtag(value, tag string) bool {
	runes := []rune(strings.ToLower(value))
	tagRunes := []rune(strings.ToLower(tag))
	for start, r := range runes {
		if r != '#' {
			continue
		}
		position := start + 1
		for position < len(runes) && unicode.Is(unicode.Cf, runes[position]) {
			position++
		}
		if position+len(tagRunes) > len(runes) {
			continue
		}
		matched := true
		for i, expected := range tagRunes {
			if runes[position+i] != expected {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		end := position + len(tagRunes)
		if end == len(runes) || (!unicode.IsLetter(runes[end]) && !unicode.IsDigit(runes[end]) && runes[end] != '_') {
			return true
		}
	}
	return false
}

func containsAnyExact(normalizedText string, terms []string) bool {
	for _, term := range terms {
		normalizedTerm := normalize(term)
		if normalizedTerm != "" && strings.Contains(" "+normalizedText+" ", " "+normalizedTerm+" ") {
			return true
		}
	}
	return false
}

func sourceAllowed(sources []db.RuleSource, peerType string, peerID int64) bool {
	if len(sources) == 0 {
		return true
	}
	if peerType == "" || peerID == 0 {
		return false
	}
	for _, source := range sources {
		if source.PeerType == peerType && source.PeerID == peerID {
			return true
		}
	}
	return false
}

func firstMatch(text string, terms []string) (bool, string, bool) {
	for _, term := range terms {
		if matched, fuzzy := termMatches(text, term); matched {
			return true, strings.TrimSpace(term), fuzzy
		}
	}
	return false, "", false
}

func firstExactMatch(text string, terms []string) (bool, string) {
	for _, term := range terms {
		if matched, recognized := numericMinimumMatches(text, term); recognized {
			if matched {
				return true, strings.TrimSpace(term)
			}
			continue
		}
		normalizedTerm := normalize(term)
		if normalizedTerm != "" && strings.Contains(" "+text+" ", " "+normalizedTerm+" ") {
			return true, strings.TrimSpace(term)
		}
	}
	return false, ""
}

func termMatches(text, term string) (bool, bool) {
	if matched, recognized := numericMinimumMatches(text, term); recognized {
		return matched, false
	}
	term = normalize(term)
	if term == "" {
		return false, false
	}
	if strings.Contains(" "+text+" ", " "+term+" ") {
		return true, false
	}
	termWords := strings.Fields(term)
	textWords := strings.Fields(text)
	if len(termWords) == 0 || len(termWords) > len(textWords) {
		return false, false
	}
	for start := 0; start+len(termWords) <= len(textWords); start++ {
		ok := true
		changed := false
		for i, expected := range termWords {
			actual := textWords[start+i]
			maxDistance := 0
			if len([]rune(expected)) >= 5 {
				maxDistance = 1
			}
			distance := editDistance(actual, expected)
			if distance > maxDistance {
				ok = false
				break
			}
			changed = changed || distance > 0
		}
		if ok && changed {
			return true, true
		}
	}
	return false, false
}

// numericMinimumMatches supports compact unit-aware terms such as
// "num>=1000:lm|lumen|lumens". The number and unit must be adjacent in the
// normalized listing text, so a price or battery capacity cannot satisfy a
// lumen requirement.
func numericMinimumMatches(text, term string) (bool, bool) {
	term = strings.TrimSpace(term)
	if !strings.HasPrefix(strings.ToLower(term), "num>=") {
		return false, false
	}
	spec := term[len("num>="):]
	rawMinimum, rawUnits, ok := strings.Cut(spec, ":")
	if !ok {
		return false, true
	}
	minimum, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(rawMinimum, ",", ".")), 64)
	if err != nil {
		return false, true
	}
	units := make([][]string, 0)
	for _, rawUnit := range strings.Split(rawUnits, "|") {
		if words := strings.Fields(normalize(rawUnit)); len(words) > 0 {
			units = append(units, words)
		}
	}
	if len(units) == 0 {
		return false, true
	}

	words := strings.Fields(text)
	for i, word := range words {
		value, err := strconv.ParseFloat(word, 64)
		if err != nil || value < minimum {
			continue
		}
		for _, unit := range units {
			if wordsAt(words, i+1, unit) {
				return true, true
			}
		}
	}
	return false, true
}

func wordsAt(words []string, start int, expected []string) bool {
	if start < 0 || start+len(expected) > len(words) {
		return false
	}
	for i, word := range expected {
		if words[start+i] != word {
			return false
		}
	}
	return true
}

func normalize(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
	var b strings.Builder
	space := true
	previousKind := 0
	for _, r := range value {
		kind := 0
		if unicode.IsLetter(r) {
			kind = 1
		} else if unicode.IsDigit(r) {
			kind = 2
		}
		if kind != 0 {
			if !space && previousKind != 0 && kind != previousKind {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
			previousKind = kind
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
		previousKind = 0
	}
	return strings.TrimSpace(b.String())
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	previous := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, ra := range ar {
		current := make([]int, len(br)+1)
		current[0] = i + 1
		for j, rb := range br {
			cost := 0
			if ra != rb {
				cost = 1
			}
			current[j+1] = min3(current[j]+1, previous[j+1]+1, previous[j]+cost)
		}
		previous = current
	}
	return previous[len(br)]
}

func min3(a, b, c int) int {
	if a < b && a < c {
		return a
	}
	if b < c {
		return b
	}
	return c
}
