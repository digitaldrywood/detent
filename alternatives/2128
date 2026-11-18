package hubapi

import (
	"strings"
	"unicode/utf8"
)

func BoundedFailureText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	end := limit
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}
