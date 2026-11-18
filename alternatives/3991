package searchstore

import (
	"strings"
	"unicode"
)

const (
	WindowRows       = 2500
	WindowTitleChars = 1024
	WindowBodyChars  = 32768
	maxQueryWordSize = 256
)

func WindowQuery(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	seen := make(map[string]bool)
	terms := make([]string, 0, min(len(words), TermLimit))
	for _, word := range words {
		word = strings.ToLower(word)
		if seen[word] || len(word) > maxQueryWordSize {
			continue
		}
		seen[word] = true
		terms = append(terms, quote(word)+":*")
		if len(terms) == TermLimit {
			break
		}
	}
	return strings.Join(terms, " | ")
}
