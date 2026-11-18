package issueorigin

import (
	"regexp"
	"strings"
)

var releaseSeries = regexp.MustCompile(`(?m)^Release stabilization: ` + "`" + `(release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))` + "`" + `$`)

func ReleaseStabilizationFingerprint(series string) string {
	return Fingerprint("release-stabilization:digitaldrywood/detent:" + series)
}

func ReleaseStabilizationSeries(body string) string {
	match := releaseSeries.FindStringSubmatch(body)
	origin, ok := Parse(body)
	if len(match) == 0 || !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || !strings.HasPrefix(origin.Source, "https://github.com/digitaldrywood/detent/actions/runs/") || origin.Fingerprint != ReleaseStabilizationFingerprint(match[1]) {
		return ""
	}
	return match[1]
}
