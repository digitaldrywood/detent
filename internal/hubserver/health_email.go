package hubserver

import (
	"regexp"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

var healthEmailPrivateText = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*(?:(?:bearer|basic)\s+)?[^\s,;]+|\b[a-z0-9_]*(?:token|secret|password|api[_-]?key)[a-z0-9_]*\s*[=:]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)|\b(?:bearer|basic)\s+[^\s,;]+|\b(?:gh[pousr]_|github_pat_|sk[-_]|AKIA|ASIA)[a-z0-9_-]+|\beyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|(?:https?://|file://)[^\s]+|\b[a-z0-9_+/=-]{32,}\b`)

var healthEmailSubjectID = regexp.MustCompile(`^(?:wi|prj|org|runner|machine|conv)_[0-9a-f]{32}$`)

func healthEmailText(value string) string {
	return hubapi.BoundedFailureText(healthEmailPrivateText.ReplaceAllStringFunc(failureFirstLine(value), func(match string) string {
		if healthEmailSubjectID.MatchString(match) {
			return match
		}
		return "[redacted]"
	}), maxFailureExampleLength)
}
