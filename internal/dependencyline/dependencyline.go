package dependencyline

import "regexp"

var pattern = regexp.MustCompile("(?i)^\\s*(?:>\\s*)?(?:[-*+]\\s+)?(?:[*_`~]+)?\\s*(?:blocked\\s+by|depends[\\s-]+on)(?:[*_`~]+)?\\s*(?::\\s*|\\s+)(?:[*_`~]+)?\\s*(.+)\\s*$")

func Match(line string) (string, bool) {
	matches := pattern.FindStringSubmatch(line)
	if len(matches) != 2 {
		return "", false
	}
	return matches[1], true
}
