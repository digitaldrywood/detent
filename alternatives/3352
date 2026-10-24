package web

import (
	"context"
	"math"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) loadBoardAttemptCosts(ctx context.Context, data *templates.DashboardData, projectID, issueID, identifier string) {
	if projectID == "" || (issueID == "" && identifier == "") {
		return
	}
	history, ok := s.store.(store.WorkAttemptStore)
	if !ok {
		return
	}
	// Scope the untruncated history to this issue, including merged PRs whose
	// attempts are no longer in the fleet snapshot's recent history window.
	attempts, err := history.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{ProjectID: projectID, IssueID: issueID, Identifier: identifier, Limit: math.MaxInt32})
	if err != nil {
		data.AttemptCostsError = "Attempt costs unavailable"
		return
	}
	for _, attempt := range attempts {
		data.AttemptCosts = append(data.AttemptCosts, templates.AttemptCost(attempt.ID, attempt.AttemptNumber, attempt.WorkerType, attempt.WorkerHost, attempt.MetricsJSON))
	}
}
