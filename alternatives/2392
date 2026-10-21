package orchestrator

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestObservePullRequestHydrationSkipsWarnsAfterSustainedFleetStarvation(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	orchestrator := &Orchestrator{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	issues := make([]connector.Issue, 11)
	for index := range issues {
		issues[index] = connector.Issue{
			Identifier: fmt.Sprintf("digitaldrywood/detent#%d", index+1),
			PullRequest: &connector.PullRequest{
				HydrationUnavailableReason: connector.PullRequestHydrationReasonRESTBudgetReserved,
			},
		}
	}

	for range 3 {
		orchestrator.observePullRequestHydrationSkips(issues)
	}
	if logs.Len() != 0 {
		t.Fatalf("logs before sustained threshold = %q, want empty", logs.String())
	}

	orchestrator.observePullRequestHydrationSkips(issues)
	got := logs.String()
	for _, want := range []string{
		"level=WARN",
		`msg="github pull request hydration starvation"`,
		"skipped_issue_count=11",
		"consecutive_tick_threshold=3",
		"max_consecutive_ticks=4",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs = %q, want %q", got, want)
		}
	}

	orchestrator.observePullRequestHydrationSkips(issues)
	if strings.Count(logs.String(), "github pull request hydration starvation") != 1 {
		t.Fatalf("logs = %q, want one warning until recovery", logs.String())
	}

	orchestrator.observePullRequestHydrationSkips(nil)
	for range 4 {
		orchestrator.observePullRequestHydrationSkips(issues)
	}
	if strings.Count(logs.String(), "github pull request hydration starvation") != 2 {
		t.Fatalf("logs = %q, want warning after starvation recurs", logs.String())
	}
}
