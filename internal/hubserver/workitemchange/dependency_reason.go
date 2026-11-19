package workitemchange

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func ValidDependencyReason(text string) bool {
	for index, char := range text {
		if (char == '.' || char == '!' || char == '?') && strings.HasPrefix(text[index+1:], " ") && strings.TrimSpace(text[index+1:]) != "" {
			return false
		}
	}
	return text == strings.TrimSpace(text) && utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl) && utf8.RuneCountInString(text) <= 140 && !strings.ContainsAny(text, "\r\n\t<>[]*_`") && !strings.Contains(text, "://") && !strings.Contains(strings.ToLower(text), "mailto:") && !strings.Contains(strings.ToLower(text), "www.")
}
