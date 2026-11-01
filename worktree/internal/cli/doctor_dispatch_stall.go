package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func checkDoctorDispatchStalls(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	check := doctorCheck{
		Name:   "Dispatch stalls",
		Status: doctorOK,
		Detail: "no projects have a sustained all-candidate dispatch stall",
	}
	stalls, err := readDoctorDispatchStalls(ctx, doctorLiveBoot(boot, &boot.Global), deps)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = "live dispatch-stall check unavailable: " + err.Error()
		return check
	}
	projectID = strings.TrimSpace(projectID)
	filtered := make([]telemetry.DispatchStatus, 0, len(stalls))
	for _, stall := range stalls {
		class := observability.Normalize(stall.Class, observability.Dispatch(stall.Stalled, stall.WaitReasonCode))
		if class == observability.ClassFault && (projectID == "" || strings.TrimSpace(stall.ProjectID) == projectID) {
			filtered = append(filtered, stall)
		}
	}
	if len(filtered) == 0 {
		return check
	}
	check.Status = doctorWarn
	check.DispatchStalls = filtered
	details := make([]string, 0, len(filtered))
	for _, stall := range filtered {
		details = append(details, doctorDispatchStallDetail(stall))
	}
	check.Detail = fmt.Sprintf("%d project dispatch stall(s) need human attention: %s", len(filtered), strings.Join(details, "; "))
	check.Hint = "Review the common wait reason and restore dispatch eligibility, then rerun detent doctor after a selection succeeds."
	return check
}

func readDoctorDispatchStalls(ctx context.Context, boot BootConfig, deps doctorDeps) ([]telemetry.DispatchStatus, error) {
	probe, err := probeDoctorHealth(ctx, boot, deps)
	if err != nil {
		return nil, err
	}
	return probe.Health.DispatchStalls, nil
}

func doctorDispatchStallDetail(stall telemetry.DispatchStatus) string {
	projectID := strings.TrimSpace(stall.ProjectID)
	if projectID == "" {
		projectID = "project"
	}
	duration := time.Duration(stall.StallDurationSeconds) * time.Second
	return fmt.Sprintf("%s stalled for %s with %d candidate(s) waiting on %s", projectID, duration, stall.CandidateCount, strings.TrimSpace(stall.WaitReason))
}
