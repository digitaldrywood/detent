package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
		check.Detail = "live dispatch-stall check skipped because no Detent instance was reachable"
		return check
	}
	projectID = strings.TrimSpace(projectID)
	filtered := make([]telemetry.DispatchStatus, 0, len(stalls))
	for _, stall := range stalls {
		if projectID == "" || strings.TrimSpace(stall.ProjectID) == projectID {
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, doctorHealthProbeURL(boot), nil)
	if err != nil {
		return nil, err
	}
	response, err := deps.httpDo(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	var health doctorHealthResponse
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return nil, err
	}
	if strings.TrimSpace(health.Mode) == "" || !doctorHealthHasDetentChecks(health.Checks) {
		return nil, errors.New("response was not Detent health")
	}
	return health.DispatchStalls, nil
}

func doctorDispatchStallDetail(stall telemetry.DispatchStatus) string {
	projectID := strings.TrimSpace(stall.ProjectID)
	if projectID == "" {
		projectID = "project"
	}
	duration := time.Duration(stall.StallDurationSeconds) * time.Second
	return fmt.Sprintf("%s stalled for %s with %d candidate(s) waiting on %s", projectID, duration, stall.CandidateCount, strings.TrimSpace(stall.WaitReason))
}
