package workspacerelay

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func ActorName(email string) string {
	address := strings.TrimSpace(email)
	if address == "" {
		return ""
	}
	local := address
	if at := strings.LastIndex(address, "@"); at > 0 {
		local = address[:at]
	}
	if plus := strings.IndexByte(local, '+'); plus > 0 {
		local = local[:plus]
	}
	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
	parts := make([]string, 0, len(words))
	for _, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		if first == utf8.RuneError {
			parts = append(parts, word)
			continue
		}
		parts = append(parts, string(unicode.ToUpper(first))+word[size:])
	}
	if len(parts) == 0 {
		return address
	}
	return strings.Join(parts, " ")
}
