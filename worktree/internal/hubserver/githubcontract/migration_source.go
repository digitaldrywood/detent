package githubcontract

import (
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var migrationSourceURL = regexp.MustCompile(`https://github\.com/[^\s<>()\[\]]+`)

func MigrationSourceURL(body string) string {
	var candidate string
	section := false
	for line := range strings.SplitSeq(body, "\n") {
		label := strings.TrimLeft(strings.TrimSpace(line), "#*- ")
		if strings.HasPrefix(strings.ToLower(label), "migration context") {
			rest := strings.TrimSpace(label[len("migration context"):])
			if rest == "" || strings.HasPrefix(rest, ":") || rest == "**" {
				section = strings.HasPrefix(strings.TrimSpace(line), "#")
			} else {
				continue
			}
		} else if strings.HasPrefix(strings.TrimSpace(line), "#") {
			section = false
			continue
		} else if !section {
			continue
		}
		for _, raw := range migrationSourceURL.FindAllString(line, -1) {
			canonical, _, _, err := tracker.ParseGitHubIssueURL(strings.TrimRight(raw, "`*.,;"))
			if err != nil {
				continue
			}
			if candidate != "" && candidate != canonical {
				return ""
			}
			candidate = canonical
		}
	}
	return candidate
}
