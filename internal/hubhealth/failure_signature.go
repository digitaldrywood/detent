package hubhealth

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxFailureSignatureLength = 256

var failurePathDigits = regexp.MustCompile(`\d+`)

var failureSignatureReplacements = []struct {
	pattern *regexp.Regexp
	value   string
}{
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ][\d:.]+(?:Z|[+-]\d{2}:?\d{2})?\b`), "<timestamp>"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<id>"},
	{regexp.MustCompile(`\b(?:wi|prj|attempt|lease|run|session|change|version|machine|org)_[a-zA-Z0-9_-]+\b`), "<id>"},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:bytes?|[kmgt]i?b)\b`), "<bytes>"},
	{regexp.MustCompile(`\b[[:xdigit:]]{7,64}\b`), "<hash>"},
	{regexp.MustCompile(`(?:[a-zA-Z]:[\\/]|[~.]?[\\/]|[a-zA-Z0-9_.-]+[\\/])[^\s"'<>]+`), "<path>"},
	{regexp.MustCompile(`\b(attempt|run|lease|session|process|pid|issue|thread|turn)([ _:#=]+)\d+\b`), "${1}${2}<id>"},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?(\s+(?:characters?|chars?|tokens?|attempts?|items?))\b`), "<n>${1}"},
	{regexp.MustCompile(`(?i)\b(actual|max|expected|got|limit|count|size|length|input|output)([ =:]+)\d+\b`), "${1}${2}<n>"},
}

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

func FailureFirstLine(value string) string {
	first, _, _ := strings.Cut(value, "\n")
	return strings.TrimSpace(first)
}

func NormalizeFailureSignature(value string) string {
	value = FailureFirstLine(value)
	for _, replacement := range failureSignatureReplacements {
		switch replacement.value {
		case "<path>":
			value = replacement.pattern.ReplaceAllStringFunc(value, func(match string) string {
				return failurePathDigits.ReplaceAllString(match, "<n>")
			})
		case "<hash>":
			value = replacement.pattern.ReplaceAllStringFunc(value, func(match string) string {
				if strings.ContainsAny(match, "abcdefABCDEF") || len(match) == 40 || len(match) == 64 {
					return replacement.value
				}
				return match
			})
		default:
			value = replacement.pattern.ReplaceAllString(value, replacement.value)
		}
	}
	return BoundedFailureText(value, maxFailureSignatureLength)
}
