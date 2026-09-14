package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func checkDoctorLaneSignals(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	check := doctorCheck{
		Name:   "Ignored lane signals",
		Status: doctorOK,
		Detail: "no open issues have lane signals ignored by their configured source",
	}
	probe, err := probeDoctorHealth(ctx, doctorLiveBoot(boot, &boot.Global), deps)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = "live ignored-lane-signal check unavailable: " + err.Error()
		return check
	}
	projectID = strings.TrimSpace(projectID)
	warnings := make([]telemetry.LaneSignalWarning, 0, len(probe.Health.LaneSignalWarnings))
	for _, warning := range probe.Health.LaneSignalWarnings {
		if projectID == "" || strings.TrimSpace(warning.ProjectID) == projectID {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) == 0 {
		return check
	}
	check.Status = doctorWarn
	check.LaneSignalWarnings = warnings
	details := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		target := strings.TrimSpace(warning.Identifier)
		if target == "" {
			target = strings.TrimSpace(warning.IssueID)
		}
		if target == "" {
			target = strings.TrimSpace(warning.IssueURL)
		}
		details = append(details, target+" reads lanes from "+strings.TrimSpace(warning.ConfiguredSource)+"; "+strings.TrimSpace(warning.Action))
	}
	check.Detail = fmt.Sprintf("%d open issue(s) have ignored lane signals: %s", len(warnings), strings.Join(details, "; "))
	check.Hint = "Use each project's configured lane source, then rerun detent doctor."
	return check
}
