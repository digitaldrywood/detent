package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) SchedulerDecision(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, id string) (eventstore.Event, error) {
	row, err := s.queries.RecordGetEvent(ctx, pgquery.RecordGetEventParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, ID: id})
	if err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	event := eventRecord(row)
	if event.Type != "scheduler.decision" {
		return eventstore.Event{}, artifact.ErrIntegrity
	}
	return event, nil
}

func (s *session) LatestClaimedSchedulerDecision(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.Event, error) {
	row, err := s.queries.RecordLatestClaimedSchedulerDecision(ctx, pgquery.RecordLatestClaimedSchedulerDecisionParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	return eventRecord(row), mapError(ctx, err)
}

func (s *session) SchedulerDecisions(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.SchedulerDecisionRows, error) {
	rows, err := s.queries.SchedulerEventHeads(ctx, pgquery.SchedulerEventHeadsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
	if err != nil {
		return eventstore.SchedulerDecisionRows{}, mapError(ctx, err)
	}
	result := eventstore.SchedulerDecisionRows{Available: len(rows) > 0}
	for _, row := range rows {
		event := eventRecord(row.CollaborationEvent)
		if event.Sequence != row.HeadSequence || event.ActorKind != row.HeadActorKind || event.ActorPrincipalID != row.HeadActorPrincipalID {
			return result, artifact.ErrIntegrity
		}
		event.SchedulerOutcome = row.HeadOutcome
		if event.OrganizationID != s.organization || event.ProjectID != project || event.WorkItemID != item || event.Sequence < 1 || event.Type != "scheduler.decision" {
			return result, artifact.ErrIntegrity
		}
		switch row.HeadKind {
		case "latest":
			result.Latest = &event
		case "excluded":
			if event.SchedulerOutcome != "skipped" {
				return result, artifact.ErrIntegrity
			}
			result.Exclusion = &event
		case "unprojected":
			if event.SchedulerOutcome != "" {
				return result, artifact.ErrIntegrity
			}
			result.Unprojected = &event
		default:
			return result, artifact.ErrIntegrity
		}
	}
	if result.Available && (result.Latest == nil || result.Exclusion != nil && result.Exclusion.Sequence > result.Latest.Sequence || result.Unprojected != nil && result.Unprojected.Sequence > result.Latest.Sequence) {
		return result, artifact.ErrIntegrity
	}
	for _, category := range []struct {
		outcome string
		row     *eventstore.Event
	}{{"skipped", result.Exclusion}, {"", result.Unprojected}} {
		if result.Latest != nil && result.Latest.SchedulerOutcome == category.outcome && (category.row == nil || *category.row != *result.Latest) {
			return result, artifact.ErrIntegrity
		}
		if category.row != nil && result.Latest != nil && category.row.ID == result.Latest.ID && *category.row != *result.Latest {
			return result, artifact.ErrIntegrity
		}
	}
	if result.Exclusion != nil && result.Unprojected != nil && result.Exclusion.ID == result.Unprojected.ID && *result.Exclusion != *result.Unprojected {
		return result, artifact.ErrIntegrity
	}
	return result, nil
}
