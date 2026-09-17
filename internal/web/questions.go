package web

import (
	"context"

	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) snapshotOpenQuestions(ctx context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot {
	reader, ok := s.store.(interface {
		OpenHumanQuestions(context.Context) ([]operations.Decision, error)
	})
	if !ok {
		return snapshot
	}
	questions, err := reader.OpenHumanQuestions(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "open question query failed", "error", err)
		return snapshot
	}
	issues := dailyDigestSnapshotIssues(snapshot)
	for i := range questions {
		q := &questions[i]
		for _, issue := range issues {
			if operationsProjectScope(issue.ProjectID, snapshot.Project.ID) == q.ProjectID && issue.Identifier == q.Issue {
				q.Title = issue.Title
				q.URL = operationsQuestionURL(q.URL, issue.URL, s.operationsDecisionHost(q.ProjectID, issue.URL))
				break
			}
		}
	}
	snapshot.OpenQuestions = questions
	return snapshot
}
