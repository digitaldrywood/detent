package dependencyline

import (
	"regexp"
	"strings"
)

var pattern = regexp.MustCompile(`(?i)^\s*(?:[-*+]\s+)?(?:[*_~]+)?\s*(?:blocked\s+by|depends[\s-]+on)(?:[*_~]+)?\s*(?::\s*|\s+)(?:[*_~]+)?\s*(.+)\s*$`)

// Keep reference-shaped invalid numbers intact for CanonicalReference validation.
const referenceToken = "(?:https?://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/issues/[A-Za-z0-9_]+|(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#[A-Za-z0-9_]+)"
const decoratedReference = "(?:\\[" + referenceToken + "\\]\\(https?://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/issues/[0-9]+\\)|`" + referenceToken + "`|" + referenceToken + ")"

var listPattern = regexp.MustCompile("(?i)^" + decoratedReference + "(?:(?:[ \t]*[,;][ \t]*(?:and[ \t]+)?|[ \t]+and[ \t]+)" + decoratedReference + ")*")

// ExplicitEmpty reports a leading empty-dependency marker, regardless of trailing prose.
func ExplicitEmpty(text string) bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '.' || r == ',' || r == ';' || r == '!' || r == '?' || r == '\r' || r == '\n'
	})
	return len(words) > 0 && (words[0] == "none" || words[0] == "n/a" || words[0] == "-")
}

// Match returns only the reference list immediately following a declaration label.
// Prose, sentence punctuation, and explicit empty declarations end the list.
func Match(line string) (string, bool) {
	text, ok := MatchText(line)
	if !ok {
		return "", false
	}
	if ExplicitEmpty(text) {
		return "", true
	}
	return listPattern.FindString(text), true
}

// MatchText retains trailing prose for callers that inspect human instructions.
func MatchText(line string) (string, bool) {
	// Normalize code spans around the label only. A code span containing the
	// entire declaration remains an example, not a dependency.
	line = strings.TrimSpace(line)
	if len(line) > 1 && strings.ContainsRune("-*+", rune(line[0])) && (line[1] == ' ' || line[1] == '\t') {
		line = strings.TrimSpace(line[1:])
	}
	if strings.HasPrefix(line, "`") {
		if end := strings.Index(line[1:], "`"); end >= 0 {
			label := line[1 : end+1]
			normalized := strings.ToLower(strings.TrimSpace(strings.TrimRight(label, ":")))
			if normalized == "depends on" || normalized == "depends-on" || normalized == "blocked by" {
				line = label + line[end+2:]
			}
		}
	}
	matches := pattern.FindStringSubmatch(line)
	if len(matches) != 2 {
		return "", false
	}
	return matches[1], true
}
