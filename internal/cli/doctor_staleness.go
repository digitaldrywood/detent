package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func checkDoctorFleetStaleness(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	check := doctorCheck{
		Name:   "Fleet staleness",
		Status: doctorOK,
		Detail: "no active fleet staleness faults",
	}
	probe, err := probeDoctorHealth(ctx, doctorLiveBoot(boot, &boot.Global), deps)
	if err != nil {
		check.Detail = "live condition check skipped because no healthy Detent instance was reachable"
		return check
	}
	projectID = strings.TrimSpace(projectID)
	warnings := make([]telemetry.StalenessWarning, 0, len(probe.Health.StalenessWarnings))
	for _, warning := range probe.Health.StalenessWarnings {
		class := observability.Normalize(warning.Class, observability.Staleness(warning.WaitingOnHuman))
		if class == observability.ClassFault && (projectID == "" || strings.TrimSpace(warning.ProjectID) == projectID) {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) == 0 {
		return check
	}
	check.Status = doctorWarn
	check.StalenessWarnings = warnings
	check.Detail = fmt.Sprintf("%d active fleet staleness fault(s); oldest: %s", len(warnings), doctorStalenessWarningDetail(warnings[0]))
	check.Hint = "Review the affected items or queues, then rerun detent doctor after work advances."
	return check
}

func doctorStalenessWarningDetail(warning telemetry.StalenessWarning) string {
	target := strings.TrimSpace(warning.Identifier)
	if target == "" {
		target = strings.TrimSpace(warning.IssueID)
	}
	if target == "" {
		target = strings.TrimSpace(warning.ProjectID)
	}
	return target + " " + strings.TrimSpace(warning.Reason)
}
