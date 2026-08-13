package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/healthnotify"
)

func checkDoctorHealthNotificationDelivery(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	check := doctorCheck{
		Name:   "Health notification delivery",
		Status: doctorOK,
		Detail: "no health notification delivery failures",
	}
	found, err := readDoctorHealthNotificationFailures(ctx, doctorLiveBoot(boot, &boot.Global), deps)
	if err != nil {
		check.Detail = "live notification check skipped because no healthy Detent instance was reachable"
		return check
	}
	projectID = strings.TrimSpace(projectID)
	failures := make([]healthnotify.Failure, 0, len(found))
	for _, failure := range found {
		if projectID == "" || strings.TrimSpace(failure.ProjectID) == "" || strings.TrimSpace(failure.ProjectID) == projectID {
			failures = append(failures, failure)
		}
	}
	if len(failures) == 0 {
		return check
	}
	check.Status = doctorWarn
	check.HealthNotificationFailures = failures
	exhausted := 0
	for _, failure := range failures {
		if failure.FailedAt != nil || failure.Attempts >= failure.MaxAttempts {
			exhausted++
		}
	}
	check.Detail = fmt.Sprintf("%d health notification delivery failure(s), %d exhausted bounded retry", len(failures), exhausted)
	check.Hint = "Restore the configured webhook receiver, then restart Detent or wait for the next retry; exhausted events remain visible for operator review."
	return check
}

func readDoctorHealthNotificationFailures(ctx context.Context, boot BootConfig, deps doctorDeps) ([]healthnotify.Failure, error) {
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
	return health.HealthNotificationFailures, nil
}
