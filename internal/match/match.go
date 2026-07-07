package match

import (
	"strings"

	"github.com/nextster/tg-radar/internal/db"
)

func Find(text string, keywords []db.Keyword) []db.Keyword {
	normalizedText := strings.ToLower(text)
	var matches []db.Keyword
	for _, keyword := range keywords {
		phrase := strings.TrimSpace(keyword.Phrase)
		if phrase == "" || !keyword.Enabled {
			continue
		}
		if strings.Contains(normalizedText, strings.ToLower(phrase)) {
			matches = append(matches, keyword)
		}
	}
	return matches
}
