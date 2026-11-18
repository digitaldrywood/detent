package hubserver

import (
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func hostedRunnerTransactionMetrics(path string) []string {
	switch path {
	case hubapi.NativeBase + "/leases/:lease/validate", hubapi.NativeBase + "/claims/preview":
		return []string{"events_total", "usage_windows"}
	case hubapi.NativeBase + "/machines/:machine/heartbeat":
		return []string{"connected_runners", "events_total", "usage_windows", "collaboration_bytes"}
	case enrollmentBase + "/redeem":
		return []string{"registered_runners", "connected_runners", "usage_windows"}
	case runnerBase + "/:runner/renew", runnerBase + "/:runner/rotate":
		return []string{"usage_windows"}
	}
	return nil
}

func hostedNativeMutationMetrics(input any, completion bool) []string {
	metrics := []string{"events_total", "usage_windows"}
	if _, runEvent := input.(tracker.NativeRunEvent); !runEvent {
		metrics = append(metrics, "unarchived_issues", "projects", "repositories", "registered_runners")
		if !completion {
			metrics = append(metrics, "connected_runners")
		}
	}
	if !completion {
		metrics = append(metrics, "collaboration_bytes", "history_records")
	}
	return metrics
}

func hostedCompletionMutation(c echo.Context, input any) bool {
	if strings.HasSuffix(c.Path(), "/work-items/:item/archive") {
		return true
	}
	if event, ok := input.(tracker.NativeRunEvent); ok {
		return event.Type == "run.finished" || event.Type == "run.checkpointed"
	}
	return false
}

func hostedMutationFeature(c echo.Context) string {
	if strings.Contains(c.Path(), "/integration") || strings.Contains(c.Path(), "/imports") {
		return "github_integration"
	}
	return "collaboration"
}
