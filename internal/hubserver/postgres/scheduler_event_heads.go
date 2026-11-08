package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) SchedulerDecisions(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.SchedulerDecisionRows, error) {
	rows, err := s.queries.SchedulerEventHeads(ctx, pgquery.SchedulerEventHeadsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: string(item)})
	if err != nil {
		return eventstore.SchedulerDecisionRows{}, mapError(ctx, err)
	}
	result := eventstore.SchedulerDecisionRows{Available: len(rows) > 0}
	for _, row := range rows {
		event := eventRecord(pgquery.CollaborationEvent{
			OrganizationID:       row.OrganizationID,
			ProjectID:            row.ProjectID,
			WorkItemID:           tracker.NativeWorkItemID(row.WorkItemID),
			ID:                   row.ID,
			Sequence:             row.Sequence,
			Type:                 row.Type,
			SchemaVersion:        row.SchemaVersion,
			ActorKind:            row.ActorKind,
			ActorPrincipalID:     row.ActorPrincipalID,
			EventRevision:        row.EventRevision,
			ChangeID:             row.ChangeID,
			VersionID:            row.VersionID,
			HeadSHA:              row.HeadSHA,
			RunID:                row.RunID,
			AttemptID:            row.AttemptID,
			FromState:            row.FromState,
			ToState:              row.ToState,
			RecordedAt:           row.RecordedAt,
			PayloadObjectKey:     row.PayloadObjectKey,
			PayloadSHA256:        row.PayloadSHA256,
			PayloadBytes:         row.PayloadBytes,
			PayloadContentBytes:  row.PayloadContentBytes,
			PayloadFormatVersion: row.PayloadFormatVersion,
			LandingWaiting:       row.LandingWaiting,
			LandingChangeID:      row.LandingChangeID,
			LandingVersionID:     row.LandingVersionID,
			LandingHeadSHA:       row.LandingHeadSHA,
			ProjectSequence:      row.ProjectSequence,
			PushKnown:            row.PushKnown,
			PushChangeID:         row.PushChangeID,
			PushVersionID:        row.PushVersionID,
			PushHeadSha:          row.PushHeadSha,
			PushExternalEffect:   row.PushExternalEffect,
			PushEffectState:      row.PushEffectState,
			SchedulerOutcome:     row.SchedulerOutcome,
		})
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
