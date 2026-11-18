package runnerupdate

import (
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/update"
)

// MinimumSupportedVersion is the oldest runner release that can still
// take work. Raise it only in a change that breaks runner and Hub
// compatibility; a Hub release alone never strands older runners.
const MinimumSupportedVersion = "0.117.56"

func Target(version string) string {
	version = strings.TrimSpace(version)
	if update.IsDevelopmentVersion(version) {
		return ""
	}
	return version
}

func MinimumVersion(version string) string {
	target := Target(version)
	if order, err := update.CompareVersions(MinimumSupportedVersion, target); err == nil && order < 0 {
		return MinimumSupportedVersion
	}
	return target
}

func ClaimRefusal(minimum, reported string) string {
	if !Behind(minimum, reported) {
		return ""
	}
	return "Too old to take work, needs " + minimum
}

func VersionError(minimum, reported string) error {
	if reason := ClaimRefusal(minimum, reported); reason != "" {
		return &hubapi.Error{Code: "unavailable", Message: reason, Status: http.StatusUpgradeRequired, Public: true}
	}
	return nil
}

func Behind(current string, reported string) bool {
	order, err := update.CompareVersions(reported, current)
	return err == nil && order < 0
}
