package hubserver

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hydrateSchedulerDecisionRows(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, evidence tracker.NativeRuntimeEvidence, rows eventstore.SchedulerDecisionRows) (tracker.NativeRuntimeEvidence, error) {
	hydrated := map[string]*tracker.CollaborationEvent{}
	read := func(row eventstore.Event) (*tracker.CollaborationEvent, error) {
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project || row.WorkItemID != item {
			return nil, corestore.ErrTenantMismatch
		}
		if event := hydrated[row.ID]; event != nil {
			return event, nil
		}
		event, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).Event(ctx, row)
		if err != nil {
			return nil, err
		}
		if event.Type != "scheduler.decision" || event.Data.Decision == nil {
			return nil, artifact.ErrIntegrity
		}
		if explanationRead(ctx) {
			event.Data.ReasonDetail = ""
		}
		hydrated[row.ID] = &event
		return &event, nil
	}
	var latest *tracker.CollaborationEvent
	if rows.Latest != nil {
		var err error
		latest, err = read(*rows.Latest)
		if err != nil {
			return evidence, err
		}
	}
	evidence.LatestDecision = latest
	evidence.NativeCandidateExclusion = nil
	complete := rows.Available && (rows.Unprojected == nil || rows.Exclusion != nil && rows.Unprojected.Sequence < rows.Exclusion.Sequence)
	if complete {
		var exclusion *tracker.CollaborationEvent
		if rows.Exclusion != nil {
			var err error
			exclusion, err = read(*rows.Exclusion)
			if err != nil {
				return evidence, err
			}
		}
		if exclusion != nil && exclusion.Data.Decision.Outcome != "skipped" {
			return evidence, artifact.ErrIntegrity
		}
		evidence.NativeCandidateExclusion = exclusion
		evidence.Unavailable = slices.DeleteFunc(evidence.Unavailable, func(field string) bool { return field == "scheduler_history_projection" })
	}
	return evidence, nil
}
