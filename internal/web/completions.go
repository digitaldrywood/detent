package web

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) snapshotShippedCompletions(ctx context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot {
	reader, ok := s.store.(store.ShippedOutcomeStore)
	if !ok {
		return snapshot
	}
	outcomes, err := reader.ShippedOutcomes(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "shipped completion query failed", "error", err)
		return snapshot
	}
	snapshot.Shipped = make([]telemetry.Completed, 0, len(outcomes))
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
		snapshot.Shipped = append(snapshot.Shipped, telemetry.Completed{Issue: issue, CompletedAt: outcome.CompletedAt, FinalState: outcome.State})
	}
	return snapshot
}
