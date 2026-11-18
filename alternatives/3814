package reports

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func formatHubTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func validateNativeIssueType(value string, required bool) error {
	if value == "" && !required || value == "bug" || value == "change_request" {
		return nil
	}
	return hubapi.PublicInvalid("type must be bug or change_request")
}
