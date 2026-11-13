package reports

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func formatHubTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}

func validateNativeIssueType(value string, required bool) error {
	if value == "" && !required || value == "bug" || value == "change_request" {
		return nil
	}
	return hubapi.PublicInvalid("type must be bug or change_request")
}
