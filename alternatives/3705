package web

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) snapshotShippedCompletions(ctx context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot {
	if _, ok := s.store.(store.ShippedOutcomeStore); !ok {
		return snapshot
	}
	now := snapshot.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	entry := s.workflowHistory.get(ctx, s.store, "", now, s.now, s.loadWorkflowHistory)
	if entry.shippedAvailable {
		snapshot.Shipped = make([]telemetry.Completed, len(entry.shipped))
		copy(snapshot.Shipped, entry.shipped)
	}
	return snapshot
}

func (s *Server) loadShippedCompletions(ctx context.Context) ([]telemetry.Completed, bool) {
	reader, ok := s.store.(store.ShippedOutcomeStore)
	if !ok {
		return nil, true
	}
	outcomes, err := reader.ShippedOutcomes(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "shipped completion query failed", "error", err)
		return nil, false
	}
	shipped := make([]telemetry.Completed, 0, len(outcomes))
	for _, outcome := range outcomes {
		issue := telemetry.Issue{
			ID: outcome.IssueID, ProjectID: outcome.ProjectID,
			Identifier: outcome.Identifier, URL: outcome.IssueURL, State: outcome.State,
		}
		if outcome.PRNumber > 0 {
			issue.PullRequest = &telemetry.PullRequest{Number: int(outcome.PRNumber), State: "MERGED"}
		}
		identifier := strings.TrimSpace(issue.Identifier)
		if identifier == "" {
			identifier = issue.ID
		}
		issue.Title = "Completed " + identifier
		shipped = append(shipped, telemetry.Completed{Issue: issue, CompletedAt: outcome.CompletedAt, FinalState: outcome.State})
	}
	return shipped, true
}
