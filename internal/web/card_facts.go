package web

import (
	"context"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) snapshotCardHistory(ctx context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot {
	snapshot.CardActiveStates = make(map[string][]string)
	for _, issue := range telemetry.CardIssues(snapshot) {
		workflow := s.kanbanWorkflow
		if s.registry != nil {
			target, _, _ := s.kanbanActionTarget(issue.ProjectID)
			workflow = target.workflow
		}
		if len(workflow.Tracker.ActiveStates) > 0 {
			snapshot.CardActiveStates[issue.ProjectID] = append([]string(nil), workflow.Tracker.ActiveStates...)
		}
	}
	reader, ok := s.store.(store.CardHistoryStore)
	if !ok {
		return snapshot
	}
	history := make(map[store.IssueIdentity]store.CardHistory)
	enrich := func(issues []telemetry.Issue, fallback string) []telemetry.Issue {
		result := append([]telemetry.Issue(nil), issues...)
		for i := range result {
			issue := &result[i]
			laneIssue := *issue
			laneIssue.State = telemetry.CardRuntimeLane(issue.State, fallback)
			if !telemetry.ActiveIssueCard(snapshot, laneIssue) {
				continue
			}
			identity := boardIssueParkIdentity(*issue)
			if identity.ProjectID == "" {
				identity.ProjectID = snapshot.Project.ID
			}
			facts, found := history[identity]
			if !found {
				var err error
				facts, err = reader.IssueCardHistory(ctx, identity, s.now())
				if err != nil {
					s.logger.WarnContext(ctx, "card history query failed", "issue_id", issue.ID, "error", err)
					continue
				}
				history[identity] = facts
			}
			issue.AttemptsToday = &facts.AttemptsToday
			issue.LaneReason = facts.LaneReason
			issue.LaneReasonAt = facts.LaneReasonAt
		}
		return result
	}
	snapshot.BoardIssues = enrich(snapshot.BoardIssues, "")
	snapshot.Pipeline = enrich(snapshot.Pipeline, "")
	snapshot.Running = append([]telemetry.Running(nil), snapshot.Running...)
	for i := range snapshot.Running {
		snapshot.Running[i].Issue = enrich([]telemetry.Issue{snapshot.Running[i].Issue}, "In Progress")[0]
	}
	snapshot.Queue = append([]telemetry.Queued(nil), snapshot.Queue...)
	for i := range snapshot.Queue {
		snapshot.Queue[i].Issue = enrich([]telemetry.Issue{snapshot.Queue[i].Issue}, "Todo")[0]
	}
	snapshot.Blocked = append([]telemetry.Blocked(nil), snapshot.Blocked...)
	for i := range snapshot.Blocked {
		snapshot.Blocked[i].Issue = enrich([]telemetry.Issue{snapshot.Blocked[i].Issue}, "Todo")[0]
	}
	return snapshot
}
