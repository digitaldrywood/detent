package postgres

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) DiagnosticsSnapshot(ctx context.Context, projects []tracker.ProjectID, from, to, now time.Time, principal string) (diagnosticstore.Snapshot, error) {
	out := diagnosticstore.Snapshot{}
	if len(projects) > 1000 || !from.Before(to) || to.Sub(from) > 90*24*time.Hour {
		return out, corestore.ErrInvalidPage
	}
	ids := make([]string, len(projects))
	for i, id := range projects {
		ids[i] = string(id)
	}
	rows, err := s.db.Query(ctx, `SELECT project_id,native_id,coalesce(workflow_state_name,''),native_created_at FROM issues WHERE organization_id=$1 AND project_id=ANY($2) AND archived=false AND ($4='' OR actor_principal_id=$4) ORDER BY id LIMIT $3`, s.organization, ids, diagnosticstore.Limit+1, principal)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row diagnosticstore.Issue
		if err := rows.Scan(&row.ProjectID, &row.WorkItemID, &row.State, &row.CreatedAt); err != nil {
			return out, mapError(ctx, err)
		}
		if row.State == "" {
			out.ResidencePartial = true
			out.Partial = true
		}
		out.Issues = append(out.Issues, row)
	}
	if err := rows.Err(); err != nil {
		return out, mapError(ctx, err)
	}
	rows.Close()
	if len(out.Issues) > diagnosticstore.Limit {
		out.Partial = true
		out.ResidencePartial = true
		out.Issues = out.Issues[:diagnosticstore.Limit]
	}
	eventTo := to
	if now.After(eventTo) {
		eventTo = now.Add(time.Microsecond)
	}
	transitions, err := s.diagnosticEvents(ctx, ids, from, eventTo, "workflow.transitioned", diagnosticstore.Limit, principal)
	if err != nil {
		return out, err
	}
	if len(transitions) > diagnosticstore.Limit {
		out.Partial = true
		out.ResidencePartial = true
		transitions = transitions[:diagnosticstore.Limit]
	}
	decisions, err := s.diagnosticEvents(ctx, ids, from, to, "scheduler.decision", diagnosticstore.ObjectLimit, principal)
	if err != nil {
		return out, err
	}
	if len(decisions) > diagnosticstore.ObjectLimit {
		out.Partial = true
		out.SchedulerPartial = true
		decisions = decisions[:diagnosticstore.ObjectLimit]
	}
	out.Events = append(transitions, decisions...)
	slices.SortFunc(out.Events, func(a, b eventstore.Event) int {
		return cmp.Or(a.RecordedAt.Compare(b.RecordedAt), cmp.Compare(a.ID, b.ID))
	})

	rows, err = s.db.Query(ctx, `SELECT l.acquired_at,least(coalesce(l.released_at,l.expires_at),l.expires_at) FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE l.organization_id=$1 AND i.project_id=ANY($2) AND ($6='' OR i.actor_principal_id=$6) AND l.acquired_at<$4 AND least(coalesce(l.released_at,l.expires_at),l.expires_at)>$3 ORDER BY l.acquired_at,l.lease_id LIMIT $5`, s.organization, ids, from, to, diagnosticstore.Limit+1, principal)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row diagnosticstore.Lease
		if err := rows.Scan(&row.AcquiredAt, &row.FinishedAt); err != nil {
			return out, mapError(ctx, err)
		}
		out.Leases = append(out.Leases, row)
	}
	if err := rows.Err(); err != nil {
		return out, mapError(ctx, err)
	}
	rows.Close()
	if len(out.Leases) > diagnosticstore.Limit {
		out.Partial = true
		out.Leases = out.Leases[:diagnosticstore.Limit]
	}
	err = s.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT a.id FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.project_id=ANY($2) AND ($6='' OR i.actor_principal_id=$6) AND a.started_at>=$3 AND a.started_at<$4 LIMIT $5) selected`, s.organization, ids, from, to, diagnosticstore.Limit+1, principal).Scan(&out.Attempts)
	if out.Attempts > diagnosticstore.Limit {
		out.Partial = true
		out.Attempts = diagnosticstore.Limit
	}
	return out, mapError(ctx, err)
}

func (s *session) diagnosticEvents(ctx context.Context, ids []string, from, to time.Time, kind string, limit int, principal string) ([]eventstore.Event, error) {
	rows, err := s.db.Query(ctx, `SELECT e.organization_id,e.project_id,e.work_item_id,e.id,e.sequence,e.type,e.schema_version,e.actor_kind,e.actor_principal_id,e.event_revision,e.from_state,e.to_state,e.recorded_at,e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes,e.payload_format_version
FROM collaboration_events e
WHERE e.organization_id=$1 AND e.project_id=ANY($2) AND e.recorded_at>=$3 AND e.recorded_at<$4 AND e.type=$5 AND ($7='' OR EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=e.organization_id AND i.project_id=e.project_id AND i.native_id=e.work_item_id AND i.actor_principal_id=$7)) ORDER BY e.recorded_at DESC,e.id DESC LIMIT $6`, s.organization, ids, from, to, kind, limit+1, principal)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	events := []eventstore.Event{}
	for rows.Next() {
		var row eventstore.Event
		if err := rows.Scan(&row.OrganizationID, &row.ProjectID, &row.WorkItemID, &row.ID, &row.Sequence, &row.Type, &row.SchemaVersion, &row.ActorKind, &row.ActorPrincipalID, &row.EventRevision, &row.FromState, &row.ToState, &row.RecordedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes, &row.PayloadFormatVersion); err != nil {
			return nil, mapError(ctx, err)
		}
		events = append(events, row)
	}
	return events, mapError(ctx, rows.Err())
}
