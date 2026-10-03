package hubserver

import (
	"net/http"

	"github.com/digitaldrywood/detent/internal/update"
)

func minimumRunnerVersion(version string) string {
	version = detentVersion(version)
	if update.IsDevelopmentVersion(version) {
		return ""
	}
	return version
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
