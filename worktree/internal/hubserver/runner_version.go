package hubserver

import (
	"net/http"

	"github.com/digitaldrywood/detent/internal/update"
)

// minimumSupportedRunnerVersion is the oldest runner release that can still
// take work. Raise it only in a change that breaks runner and Hub
// compatibility; a Hub release alone never strands older runners.
const minimumSupportedRunnerVersion = "0.117.56"

func runnerUpdateTarget(version string) string {
	version = detentVersion(version)
	if update.IsDevelopmentVersion(version) {
		return ""
	}
	return version
}

func minimumRunnerVersion(version string) string {
	target := runnerUpdateTarget(version)
	if order, err := update.CompareVersions(minimumSupportedRunnerVersion, target); err == nil && order < 0 {
		return minimumSupportedRunnerVersion
	}
	return target
}

func runnerClaimRefusal(minimum, reported string) string {
	if !runnerBehind(minimum, reported) {
		return ""
	}
	return "Too old to take work, needs " + minimum
}

func runnerVersionError(minimum, reported string) error {
	if reason := runnerClaimRefusal(minimum, reported); reason != "" {
		return &nativeError{Code: "unavailable", Message: reason, status: http.StatusUpgradeRequired}
	}
	return nil
}
