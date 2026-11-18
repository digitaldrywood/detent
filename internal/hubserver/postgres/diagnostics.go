package postgres

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
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
	rows, err := s.db.Query(ctx, `WITH open_issues AS MATERIALIZED (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2) AND NOT i.archived ORDER BY i.project_id,i.workflow_state_name,i.id LIMIT $5)
SELECT i.project_id,i.native_id,coalesce(i.workflow_state_name,''),i.native_created_at,(SELECT count(*) FROM open_issues)>=$5
FROM open_issues c CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=$1 AND i.id=c.id LIMIT 1) i
WHERE $4='' OR i.actor_principal_id=$4 ORDER BY c.id LIMIT $3`, s.organization, ids, diagnosticstore.Limit+1, principal, maxOverviewCandidateRows+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row diagnosticstore.Issue
		var truncated bool
		if err := rows.Scan(&row.ProjectID, &row.WorkItemID, &row.State, &row.CreatedAt, &truncated); err != nil {
			return out, mapError(ctx, err)
		}
		if truncated || row.State == "" {
			out.ResidencePartial, out.Partial = true, true
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
	transitions, transitionsClipped, err := s.diagnosticEvents(ctx, `SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=ANY($2) AND e.type='workflow.transitioned' AND e.recorded_at>=$3 AND e.recorded_at<$4 ORDER BY e.project_id DESC,e.recorded_at DESC,e.sequence DESC,e.id DESC LIMIT $7`, `(SELECT count(*) FROM discovered)>=$7`, ids, from, eventTo, diagnosticstore.Limit, principal, maxOverviewCandidateRows+1)
	if err != nil {
		return out, err
	}
	if transitionsClipped || len(transitions) > diagnosticstore.Limit {
		out.Partial, out.ResidencePartial = true, true
		transitions = transitions[:min(len(transitions), diagnosticstore.Limit)]
	}
	decisions, decisionsClipped, err := s.diagnosticEvents(ctx, `SELECT e.* FROM (SELECT p.project_id FROM unnest($2::text[]) p(project_id) LIMIT $7) p CROSS JOIN LATERAL (SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=p.project_id AND e.type='scheduler.decision' AND e.recorded_at>=$3 AND e.recorded_at<$4 ORDER BY e.recorded_at DESC,e.id DESC LIMIT $5) e`, `$6<>'' AND EXISTS(SELECT 1 FROM discovered GROUP BY project_id HAVING count(*)>=$5)`, ids, from, to, diagnosticstore.ObjectLimit, principal, len(ids))
	if err != nil {
		return out, err
	}
	if decisionsClipped || len(decisions) > diagnosticstore.ObjectLimit {
		out.Partial, out.SchedulerPartial = true, true
		decisions = decisions[:min(len(decisions), diagnosticstore.ObjectLimit)]
	}
	out.Events = append(transitions, decisions...)
	slices.SortFunc(out.Events, func(a, b eventstore.Event) int {
		return cmp.Or(a.RecordedAt.Compare(b.RecordedAt), cmp.Compare(a.ID, b.ID))
	})

	rows, err = s.db.Query(ctx, `WITH RECURSIVE lease_machines AS ( SELECT first.machine_id,1 AS depth FROM (SELECT l.machine_id FROM leases l WHERE l.organization_id=$1 ORDER BY l.machine_id LIMIT 1) first UNION ALL SELECT next.machine_id,m.depth+1 FROM lease_machines m CROSS JOIN LATERAL ( SELECT l.machine_id FROM leases l WHERE l.organization_id=$1 AND l.machine_id>m.machine_id ORDER BY l.machine_id LIMIT 1 ) next WHERE m.depth<=$8 ), windowed AS MATERIALIZED ( SELECT l.lease_id FROM lease_machines m CROSS JOIN LATERAL ( SELECT l.lease_id FROM leases l WHERE l.organization_id=$1 AND l.machine_id=m.machine_id AND l.expires_at>$3 ORDER BY l.expires_at LIMIT $7 ) l WHERE m.depth<=$8 LIMIT $7 )
SELECT l.acquired_at,least(coalesce(l.released_at,l.expires_at),l.expires_at),EXISTS(SELECT 1 FROM lease_machines WHERE depth>$8) OR (SELECT count(*) FROM windowed)>=$7
FROM windowed w CROSS JOIN LATERAL (SELECT * FROM leases l WHERE l.organization_id=$1 AND l.lease_id=w.lease_id LIMIT 1) l
CROSS JOIN LATERAL (SELECT i.project_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.id=l.issue_id LIMIT 1) i
WHERE i.project_id=ANY($2) AND ($6='' OR i.actor_principal_id=$6) AND l.acquired_at<$4 AND least(coalesce(l.released_at,l.expires_at),l.expires_at)>$3 ORDER BY l.acquired_at,l.lease_id LIMIT $5`, s.organization, ids, from, to, diagnosticstore.Limit+1, principal, maxOverviewCandidateRows+1, maxReportProjects)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row diagnosticstore.Lease
		var truncated bool
		if err := rows.Scan(&row.AcquiredAt, &row.FinishedAt, &truncated); err != nil {
			return out, mapError(ctx, err)
		}
		out.Partial = out.Partial || truncated
		out.Leases = append(out.Leases, row)
	}
	if err := rows.Err(); err != nil {
		return out, mapError(ctx, err)
	}
	rows.Close()
	if len(out.Leases) > diagnosticstore.Limit {
		out.Partial, out.Leases = true, out.Leases[:diagnosticstore.Limit]
	}
	if err := s.diagnosticAttempts(ctx, ids, from, to, principal, &out); err != nil {
		return out, err
	}
	if err := s.diagnosticUsage(ctx, &out); err != nil {
		return out, err
	}
	if err := s.diagnosticLandings(ctx, ids, from, to, principal, &out); err != nil {
		return out, err
	}
	out.PriorDecisions = make(map[string]eventstore.Event)
	for _, event := range decisions {
		prior, err := s.queries.RecordListEventBefore(ctx, pgquery.RecordListEventBeforeParams{OrganizationID: s.organization, ProjectID: event.ProjectID, WorkItemID: event.WorkItemID, Before: event.Sequence, PageLimit: 1})
		if err != nil {
			return out, mapError(ctx, err)
		}
		if len(prior) > 0 {
			out.PriorDecisions[event.ID] = eventRecord(prior[0])
		}
	}
	return out, nil
}

const diagnosticEventPrincipal = `($6='' OR EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=e.organization_id AND i.project_id=e.project_id AND i.native_id=e.work_item_id AND i.actor_principal_id=$6))`

func (s *session) diagnosticEvents(ctx context.Context, source, clip string, ids []string, from, to time.Time, limit int, principal string, bound int) ([]eventstore.Event, bool, error) {
	rows, err := s.db.Query(ctx, `WITH discovered AS MATERIALIZED (`+source+`) SELECT `+diagnosticEventPrincipal+` AS owned,`+clip+`,e.organization_id,e.project_id,e.work_item_id,e.id,e.sequence,e.type,e.schema_version,e.actor_kind,e.actor_principal_id,e.event_revision,e.from_state,e.to_state,e.recorded_at,e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes,e.payload_format_version,e.run_id,e.attempt_id
FROM discovered e ORDER BY owned DESC,e.recorded_at DESC,e.id DESC LIMIT $5`, s.organization, ids, from, to, limit+1, principal, bound)
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	defer rows.Close()
	events, clipped := []eventstore.Event{}, false
	for rows.Next() {
		var row eventstore.Event
		var owned bool
		if err := rows.Scan(&owned, &clipped, &row.OrganizationID, &row.ProjectID, &row.WorkItemID, &row.ID, &row.Sequence, &row.Type, &row.SchemaVersion, &row.ActorKind, &row.ActorPrincipalID, &row.EventRevision, &row.FromState, &row.ToState, &row.RecordedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes, &row.PayloadFormatVersion, &row.RunID, &row.AttemptID); err != nil {
			return nil, false, mapError(ctx, err)
		}
		if owned {
			events = append(events, row)
		}
	}
	return events, clipped, mapError(ctx, rows.Err())
}

const diagnosticsAttemptCandidatesQuery = `WITH completed AS MATERIALIZED (
 SELECT a.id,a.issue_id,a.status,a.started_at,a.updated_at FROM native_attempts a
 WHERE a.organization_id=$1 AND a.status<>'running' AND a.updated_at>=$3 ORDER BY a.updated_at DESC,a.id DESC LIMIT $7
),running AS MATERIALIZED (
 SELECT a.id,a.issue_id,a.status,a.started_at,a.updated_at FROM native_attempts a
 WHERE a.organization_id=$1 AND a.status='running'
 ORDER BY a.id LIMIT $7
),candidates AS MATERIALIZED (
 SELECT * FROM completed UNION ALL SELECT * FROM running
),selected AS MATERIALIZED (
 SELECT a.*,i.project_id,i.native_id FROM candidates a CROSS JOIN LATERAL (SELECT i.project_id,i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.id=a.issue_id LIMIT 1) i
 WHERE i.project_id=ANY($2) AND a.started_at<$4 AND a.updated_at>=$3 AND ($6='' OR i.actor_principal_id=$6)
 ORDER BY a.updated_at DESC,a.id DESC LIMIT $5
)
SELECT '' AS id,'' AS project_id,'' AS native_id,'' AS status,$3::timestamptz AS started_at,$3::timestamptz AS updated_at,NULL::timestamptz AS failed_at,(SELECT count(*) FROM completed)>$8 OR (SELECT count(*) FROM running)>$8 AS clipped
UNION ALL
SELECT a.id,a.project_id,a.native_id,a.status,a.started_at,a.updated_at,outcome.recorded_at,false
FROM selected a LEFT JOIN LATERAL (SELECT f.outcome_event_id FROM report_attempt_facts f WHERE f.organization_id=$1 AND f.attempt_id=a.id LIMIT 1) f ON true
LEFT JOIN LATERAL (SELECT outcome.recorded_at FROM collaboration_events outcome WHERE outcome.organization_id=$1 AND outcome.id=f.outcome_event_id LIMIT 1) outcome ON true
ORDER BY updated_at DESC,id DESC`

func (s *session) diagnosticAttempts(ctx context.Context, projects []string, from, to time.Time, principal string, out *diagnosticstore.Snapshot) error {
	rows, err := s.db.Query(ctx, diagnosticsAttemptCandidatesQuery, s.organization, projects, from, to, diagnosticstore.AttemptLimit+1, principal, diagnosticstore.Limit+1, diagnosticstore.Limit)
	if err != nil {
		return mapError(ctx, err)
	}
	for rows.Next() {
		var attempt diagnosticstore.Attempt
		var clipped bool
		if err := rows.Scan(&attempt.ID, &attempt.ProjectID, &attempt.WorkItemID, &attempt.Status, &attempt.StartedAt, &attempt.UpdatedAt, &attempt.FailureAt, &clipped); err != nil {
			rows.Close()
			return mapError(ctx, err)
		}
		if clipped {
			out.Partial = true
			out.AttemptsPartial = true
		}
		if attempt.ID == "" {
			continue
		}
		out.AttemptRecords = append(out.AttemptRecords, attempt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if len(out.AttemptRecords) > diagnosticstore.AttemptLimit {
		out.AttemptRecords = out.AttemptRecords[:diagnosticstore.AttemptLimit]
		out.Partial = true
		out.AttemptsPartial = true
	}
	out.Attempts = len(out.AttemptRecords)
	byProject := make(map[tracker.ProjectID][]string)
	positions := make(map[string]int, len(out.AttemptRecords))
	for index, attempt := range out.AttemptRecords {
		byProject[attempt.ProjectID] = append(byProject[attempt.ProjectID], attempt.ID)
		positions[attempt.ID] = index
	}
	for project, ids := range byProject {
		events, err := s.queries.RecordLatestRunEventsByAttempts(ctx, pgquery.RecordLatestRunEventsByAttemptsParams{OrganizationID: s.organization, ProjectID: string(project), AttemptIds: ids})
		if err != nil {
			return mapError(ctx, err)
		}
		for _, event := range events {
			index, ok := positions[event.AttemptID]
			if !ok || event.WorkItemID != out.AttemptRecords[index].WorkItemID {
				return eventstore.ErrInvalidRecord
			}
			out.AttemptRecords[index].Event = eventRecord(event)
		}
	}
	for _, attempt := range out.AttemptRecords {
		if attempt.Event.ID == "" {
			out.Partial = true
			out.AttemptsPartial = true
		}
	}
	return nil
}

func (s *session) diagnosticUsage(ctx context.Context, out *diagnosticstore.Snapshot) error {
	ids, owners := make([]string, len(out.AttemptRecords)), make([]string, len(out.AttemptRecords))
	for index, attempt := range out.AttemptRecords {
		ids[index], owners[index] = attempt.ID, string(attempt.ProjectID)
	}
	rows, err := s.db.Query(ctx, `SELECT u.attempt_id,u.period,u.cost,u.tokens,u.complete,u.owned FROM (SELECT u.attempt_id,u.period,u.provider,u.model,u.cost,u.tokens,u.complete,u.project_id=r.project_id AND upper(u.currency)='USD' AS owned
 FROM (SELECT r.attempt_id,r.project_id FROM unnest($2::text[],$3::text[]) r(attempt_id,project_id) ORDER BY r.attempt_id LIMIT $4) r
 LEFT JOIN LATERAL (SELECT true AS present FROM report_usage_current c WHERE c.organization_id=$1 AND c.attempt_id=r.attempt_id LIMIT 1) c ON true
 CROSS JOIN LATERAL (SELECT u.attempt_id,u.project_id,u.period,u.provider,u.model,u.input+u.output AS tokens,u.cost_estimate AS cost,u.complete,'USD' AS currency FROM report_usage_current u WHERE c.present AND u.organization_id=$1 AND u.attempt_id=r.attempt_id
 UNION ALL SELECT u.attempt_id,u.project_id,u.period,u.provider,u.model,u.input+u.output,u.cost_estimate,true,u.currency FROM attempt_usage u WHERE c.present IS NULL AND u.organization_id=$1 AND u.attempt_id=r.attempt_id) u LIMIT $5
) u ORDER BY u.attempt_id,u.period,u.provider,u.model`, s.organization, ids, owners, len(ids), diagnosticstore.Limit+1)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	scanned := 0
	for rows.Next() {
		var usage diagnosticstore.Usage
		var owned bool
		if err := rows.Scan(&usage.AttemptID, &usage.Period, &usage.Cost, &usage.Tokens, &usage.Complete, &owned); err != nil {
			return mapError(ctx, err)
		}
		if scanned++; !owned {
			continue
		}
		out.Usage = append(out.Usage, usage)
		out.Partial, out.UsagePartial = out.Partial || !usage.Complete, out.UsagePartial || !usage.Complete
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if scanned > diagnosticstore.Limit {
		out.Usage = out.Usage[:min(len(out.Usage), diagnosticstore.Limit)]
		out.Partial, out.UsagePartial = true, true
	}
	return nil
}

const diagnosticsLandingCandidatesQuery = `WITH landed AS MATERIALIZED (SELECT c.id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=ANY($2) AND c.landed_at>=$3 AND c.landed_at<$4 ORDER BY c.project_id DESC,c.landed_at DESC,c.id DESC LIMIT $7
),selected AS MATERIALIZED (SELECT c.organization_id,c.project_id,c.work_item_id,c.id,c.landed_at FROM landed l CROSS JOIN LATERAL (SELECT * FROM change_requests c WHERE c.organization_id=$1 AND c.id=l.id LIMIT 1) c
 CROSS JOIN LATERAL (SELECT i.actor_principal_id FROM issues i WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id LIMIT 1) i CROSS JOIN LATERAL (SELECT v.head_sha FROM change_versions v WHERE v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id LIMIT 1) v
 WHERE ($6='' OR i.actor_principal_id=$6) AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
 ORDER BY c.landed_at DESC,c.id DESC LIMIT $5
) SELECT '' AS id,'' AS work_item_id,$3::timestamptz AS landed_at,NULL::timestamptz AS recorded_at,(SELECT count(*) FROM landed)>=$7 AS clipped UNION ALL SELECT c.id,c.work_item_id,c.landed_at,entry.recorded_at,false FROM selected c LEFT JOIN LATERAL (
 SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=c.organization_id AND e.project_id=c.project_id AND e.work_item_id=c.work_item_id AND e.type='workflow.transitioned' AND e.to_state='Merging' AND e.recorded_at<=c.landed_at ORDER BY e.recorded_at DESC,e.id DESC LIMIT 1
) entry ON true ORDER BY landed_at DESC,id DESC`

func (s *session) diagnosticLandings(ctx context.Context, projects []string, from, to time.Time, principal string, out *diagnosticstore.Snapshot) error {
	rows, err := s.db.Query(ctx, diagnosticsLandingCandidatesQuery, s.organization, projects, from, to, diagnosticstore.AttemptLimit+1, principal, maxOverviewCandidateRows+1)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var landing diagnosticstore.Landing
		var clipped bool
		if err := rows.Scan(&landing.ChangeID, &landing.WorkItemID, &landing.At, &landing.EnteredAt, &clipped); err != nil {
			return mapError(ctx, err)
		}
		if out.Partial = out.Partial || clipped; landing.ChangeID != "" {
			out.Landings = append(out.Landings, landing)
		}
	}
	if err := rows.Err(); err != nil {
		return mapError(ctx, err)
	}
	if len(out.Landings) > diagnosticstore.AttemptLimit {
		out.Partial, out.Landings = true, out.Landings[:diagnosticstore.AttemptLimit]
	}
	return nil
}

func (s *session) ReadAnalyticsFailurePage(ctx context.Context, q diagnosticstore.AnalyticsFailureQuery) (diagnosticstore.AnalyticsFailurePage, error) {
	out := diagnosticstore.AnalyticsFailurePage{Attempts: []diagnosticstore.Attempt{}, Partial: q.After.ID != ""}
	if q.ProjectID == "" || q.Limit < 1 || q.Limit > 1000 || !q.From.Before(q.To) || (q.After.ID == "") != q.After.At.IsZero() {
		return out, corestore.ErrInvalidPage
	}
	args := []any{s.organization, q.ProjectID, q.From.UTC(), q.To.UTC(), q.Limit + 1, q.Principal, q.WorkItemID}
	seek := ""
	if q.After.ID != "" {
		seek = " AND (e.recorded_at,e.id)<($8,$9)"
		args = append(args, q.After.At.UTC(), q.After.ID)
	}
	rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED(
 SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.type IN('run.observed','run.checkpointed','run.finished') AND $3::timestamptz<$4`+seek+` ORDER BY e.recorded_at DESC,e.id DESC LIMIT $5
 ),attempt_metadata AS MATERIALIZED(
 SELECT requested.attempt_id,a.project_id,a.issue_id,a.lease_id,a.status,a.started_at,a.updated_at FROM (SELECT DISTINCT attempt_id FROM examined WHERE attempt_id<>'')requested LEFT JOIN LATERAL(SELECT a.project_id,a.issue_id,a.lease_id,a.status,a.started_at,a.updated_at FROM native_attempts a WHERE a.organization_id=$1 AND a.id=requested.attempt_id LIMIT 1)a ON true
 ),item_metadata AS MATERIALIZED(
 SELECT requested.project_id,requested.issue_id,i.native_id,i.actor_principal_id FROM (SELECT DISTINCT project_id,issue_id FROM attempt_metadata WHERE issue_id IS NOT NULL)requested LEFT JOIN LATERAL(SELECT i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=requested.project_id AND i.id=requested.issue_id LIMIT 1)i ON true
 ),attempt_facts AS MATERIALIZED(
 SELECT requested.attempt_id,f.latest_event_id,f.outcome_event_id FROM (SELECT DISTINCT attempt_id FROM examined WHERE attempt_id<>'')requested LEFT JOIN LATERAL(SELECT f.latest_event_id,f.outcome_event_id FROM report_attempt_facts f WHERE f.organization_id=$1 AND f.attempt_id=requested.attempt_id LIMIT 1)f ON true
 ) SELECT e.*,a.status AS attempt_status,a.started_at AS attempt_started_at,a.updated_at AS attempt_updated_at,coalesce(la.runner_id,'') AS attempt_runner_id,
 coalesce(i.native_id,'') AS item_id,
 coalesce(a.project_id=$2 AND i.native_id=e.work_item_id AND ($6='' OR coalesce(i.actor_principal_id,'')=$6) AND ($7='' OR i.native_id=$7) AND f.failed AND (af.outcome_event_id=e.id OR af.outcome_event_id IS NULL AND af.latest_event_id=e.id),false) AS eligible,
 af.latest_event_id IS NULL OR f.event_id IS NULL AS projection_missing
 FROM examined e LEFT JOIN attempt_metadata a ON a.attempt_id=e.attempt_id LEFT JOIN item_metadata i ON i.project_id=a.project_id AND i.issue_id=a.issue_id LEFT JOIN attempt_facts af ON af.attempt_id=e.attempt_id
 LEFT JOIN LATERAL(SELECT la.runner_id FROM runner_lease_assignments la WHERE la.organization_id=$1 AND la.lease_id=a.lease_id LIMIT 1)la ON true
 LEFT JOIN LATERAL(SELECT f.event_id,f.failed FROM report_event_facts f WHERE f.organization_id=$1 AND f.event_id=e.id LIMIT 1)f ON true
 ORDER BY e.recorded_at DESC,e.id DESC`, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type candidate struct {
		pgquery.CollaborationEvent
		AttemptStatus                      *string
		AttemptRunnerID                    string
		AttemptStartedAt, AttemptUpdatedAt *time.Time
		ItemID                             tracker.NativeWorkItemID
		Eligible, ProjectionMissing        bool
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
	if err != nil {
		return out, mapError(ctx, err)
	}
	more := len(candidates) > q.Limit
	candidates = candidates[:min(len(candidates), q.Limit)]
	last := diagnosticstore.AnalyticsFailureCursor{}
	seen := map[string]bool{}
	for _, row := range candidates {
		last = diagnosticstore.AnalyticsFailureCursor{At: row.RecordedAt.UTC(), ID: row.ID}
		out.ProjectionMissing = out.ProjectionMissing || row.ProjectionMissing
		if !row.Eligible {
			continue
		}
		if row.AttemptStatus == nil || row.AttemptStartedAt == nil || row.AttemptUpdatedAt == nil || seen[row.AttemptID] {
			return out, eventstore.ErrInvalidRecord
		}
		seen[row.AttemptID] = true
		at := row.RecordedAt.UTC()
		out.Attempts = append(out.Attempts, diagnosticstore.Attempt{RunnerID: row.AttemptRunnerID, ProjectID: q.ProjectID, WorkItemID: row.ItemID, ID: row.AttemptID, Status: *row.AttemptStatus, StartedAt: row.AttemptStartedAt.UTC(), UpdatedAt: row.AttemptUpdatedAt.UTC(), FailureAt: &at})
	}
	if more {
		out.Next = last
		out.Partial = true
	}
	out.Partial = out.Partial || out.ProjectionMissing
	ids := make([]string, len(out.Attempts))
	positions := map[string]int{}
	for i, attempt := range out.Attempts {
		ids[i] = attempt.ID
		positions[attempt.ID] = i
	}
	if len(ids) > 0 {
		events, err := s.LatestRunEvents(ctx, q.ProjectID, ids)
		if err != nil {
			return out, err
		}
		for _, event := range events {
			index, ok := positions[event.AttemptID]
			if !ok || event.OrganizationID != s.organization || event.ProjectID != q.ProjectID || event.WorkItemID != out.Attempts[index].WorkItemID {
				return out, eventstore.ErrInvalidRecord
			}
			out.Attempts[index].Event = event
		}
		for _, attempt := range out.Attempts {
			if attempt.Event.ID == "" {
				out.Partial = true
				out.ProjectionMissing = true
			}
		}
	}
	return out, nil
}
