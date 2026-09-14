package dependencyline

import (
	"regexp"
	"strings"
)

var pattern = regexp.MustCompile(`(?i)^\s*(?:[-*+]\s+)?(?:[*_~]+)?\s*(?:blocked\s+by|depends[\s-]+on)(?:[*_~]+)?\s*(?::\s*|\s+)(?:[*_~]+)?\s*(.+)\s*$`)

func Match(line string) (string, bool) {
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
