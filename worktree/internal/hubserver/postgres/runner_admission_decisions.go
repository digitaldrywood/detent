package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxRunnerAdmissionDecisions = 256

func (s *session) PendingRunnerAdmissionDecisions(ctx context.Context, project tracker.ProjectID, decisions []eventstore.RunnerAdmissionDecision) ([]eventstore.RunnerAdmissionDecision, error) {
	if project == "" || len(decisions) > maxRunnerAdmissionDecisions {
		return nil, corestore.ErrInvalidPage
	}
	if len(decisions) == 0 {
		return nil, nil
	}
	items := make([]string, len(decisions))
	revisions := make([]int64, len(decisions))
	observed := make([]time.Time, len(decisions))
	ids := make([]string, len(decisions))
	for i, decision := range decisions {
		if decision.WorkItemID == "" || decision.Revision < 1 || decision.ObservedAt.IsZero() || decision.EventID == "" {
			return nil, corestore.ErrInvalidPage
		}
		items[i], revisions[i], observed[i], ids[i] = string(decision.WorkItemID), decision.Revision, decision.ObservedAt, decision.EventID
	}
	rows, err := s.db.Query(ctx, `SELECT a.work_item_id,a.revision,a.observed_at,a.event_id
FROM unnest($3::text[],$4::bigint[],$5::timestamptz[],$6::text[]) AS a(work_item_id,revision,observed_at,event_id)
JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=a.work_item_id AND i.revision=a.revision
WHERE NOT EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=a.work_item_id AND e.id=a.event_id)
AND NOT EXISTS(SELECT 1 FROM (SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=a.work_item_id AND e.type='scheduler.decision' ORDER BY e.sequence DESC LIMIT 1) latest WHERE latest.recorded_at>=a.observed_at)
ORDER BY a.observed_at DESC,a.work_item_id`, s.organization, project, items, revisions, observed, ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var pending []eventstore.RunnerAdmissionDecision
	for rows.Next() {
		var decision eventstore.RunnerAdmissionDecision
		var item string
		if err := rows.Scan(&item, &decision.Revision, &decision.ObservedAt, &decision.EventID); err != nil {
			return nil, mapError(ctx, err)
		}
		decision.WorkItemID = tracker.NativeWorkItemID(item)
		decision.ObservedAt = decision.ObservedAt.UTC()
		pending = append(pending, decision)
	}
	return pending, mapError(ctx, rows.Err())
}

func (s *session) LockRunnerAdmissionItems(ctx context.Context, project tracker.ProjectID, items []tracker.NativeWorkItemID) error {
	if s.readOnly || project == "" || len(items) == 0 || len(items) > maxRunnerAdmissionDecisions {
		return corestore.ErrInvalidPage
	}
	ids := make([]string, len(items))
	for i, item := range items {
		if item == "" {
			return corestore.ErrInvalidPage
		}
		ids[i] = string(item)
	}
	slices.Sort(ids)
	rows, err := s.db.Query(ctx, `SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=ANY($3::text[]) ORDER BY id FOR NO KEY UPDATE`, s.organization, project, ids)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	return mapError(ctx, rows.Err())
}
