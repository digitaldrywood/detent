package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportreads"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReadEscapeFacts(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.EscapeFact, error) {
	args, err := s.reportQueryArgs(query)
	if err != nil {
		return nil, err
	}
	return reportreads.EscapeFacts(ctx, s.db, s.organization, args, query.Limit)
}

func (s *session) ReadAnalyticsWorkflowPage(ctx context.Context, q eventstore.AnalyticsWorkflowQuery) (eventstore.AnalyticsWorkflowPage, error) {
	out := eventstore.AnalyticsWorkflowPage{Items: []eventstore.AnalyticsWorkflowRow{}, Partial: q.After.ID != ""}
	if q.ProjectID == "" || q.Limit < 1 || q.Limit > 1000 || !q.From.Before(q.To) || (q.After.ID == "") != q.After.At.IsZero() || q.After.Sequence < 0 {
		return out, corestore.ErrInvalidPage
	}
	args := []any{s.organization, q.ProjectID, q.From.UTC(), q.To.UTC(), q.Limit + 1, q.Principal}
	seek := ""
	if q.After.ID != "" {
		seek = " AND (e.recorded_at,e.sequence,e.id)>($7,$8,$9)"
		args = append(args, q.After.At.UTC(), q.After.Sequence, q.After.ID)
	}
	rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED (
 SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.type='workflow.transitioned' AND e.recorded_at>=$3 AND e.recorded_at<$4`+seek+`
 ORDER BY e.recorded_at,e.sequence,e.id LIMIT $5
 ),item_metadata AS MATERIALIZED(
 SELECT requested.work_item_id,i.native_id,i.actor_principal_id FROM (SELECT DISTINCT work_item_id FROM examined)requested LEFT JOIN LATERAL(SELECT i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=requested.work_item_id LIMIT 1)i ON true
 ) SELECT e.*,coalesce(ws.terminal,false) AS target_terminal,i.native_id IS NOT NULL AND ($6='' OR coalesce(i.actor_principal_id,'')=$6) AS eligible
 FROM examined e LEFT JOIN item_metadata i ON i.work_item_id=e.work_item_id
 LEFT JOIN project_workflow_states ws ON ws.organization_id=$1 AND ws.project_id=$2 AND ws.name=e.to_state
 ORDER BY e.recorded_at,e.sequence,e.id`, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type candidate struct {
		pgquery.CollaborationEvent
		TargetTerminal, Eligible bool
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
	if err != nil {
		return out, mapError(ctx, err)
	}
	more := len(candidates) > q.Limit
	candidates = candidates[:min(len(candidates), q.Limit)]
	var last eventstore.AnalyticsWorkflowCursor
	for _, row := range candidates {
		last = eventstore.AnalyticsWorkflowCursor{At: row.RecordedAt.UTC(), Sequence: row.Sequence, ID: row.ID}
		if row.Eligible {
			out.Items = append(out.Items, eventstore.AnalyticsWorkflowRow{Event: eventRecord(row.CollaborationEvent), ToTerminal: row.TargetTerminal})
		}
	}
	if more {
		out.Next = last
		out.Partial = true
	}
	return out, nil
}

func (s *session) ReadAnalyticsLandingPage(ctx context.Context, q eventstore.AnalyticsLandingQuery) (eventstore.AnalyticsLandingPage, error) {
	out := eventstore.AnalyticsLandingPage{Items: []eventstore.VersionLanding{}, Partial: q.After.ChangeID != ""}
	if q.ProjectID == "" || q.Limit < 1 || q.Limit > 1000 || !q.From.Before(q.To) || (q.After.ChangeID == "") != q.After.At.IsZero() || q.AllVersions && q.After.ChangeID != "" && q.After.VersionID == "" || len(q.ReopenedItems) > 1000 || len(q.EscapedVersions) > 1000 {
		return out, corestore.ErrInvalidPage
	}
	args := []any{s.organization, q.ProjectID, q.From.UTC(), q.To.UTC(), q.Limit + 1, q.Principal}
	seek := ""
	if !q.AllVersions {
		if q.After.ChangeID != "" {
			seek = " AND (c.landed_at,c.id)>($7,$8)"
			args = append(args, q.After.At.UTC(), q.After.ChangeID)
		}
		rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED (
   SELECT c.* FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=$2 AND c.landed_at IS NOT NULL AND c.landed_at>=$3 AND c.landed_at<$4`+seek+` ORDER BY c.landed_at,c.id LIMIT $5
  ),item_metadata AS MATERIALIZED(
   SELECT requested.work_item_id,i.native_id,i.actor_principal_id FROM (SELECT DISTINCT work_item_id FROM examined)requested LEFT JOIN LATERAL(SELECT i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=requested.work_item_id LIMIT 1)i ON true
  ) SELECT c.*,i.native_id IS NOT NULL AND ($6='' OR coalesce(i.actor_principal_id,'')=$6) AND coalesce(c.landed_head_sha=v.head_sha AND length(c.landed_merge_sha) IN(40,64),false) AS eligible
  FROM examined c LEFT JOIN item_metadata i ON i.work_item_id=c.work_item_id
  LEFT JOIN change_versions v ON v.organization_id=$1 AND v.change_id=c.id AND v.id=c.landed_version_id ORDER BY c.landed_at,c.id`, args...)
		if err != nil {
			return out, mapError(ctx, err)
		}
		type candidate struct {
			pgquery.ChangeRequest
			Eligible bool
		}
		candidates, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
		if err != nil {
			return out, mapError(ctx, err)
		}
		more := len(candidates) > q.Limit
		candidates = candidates[:min(len(candidates), q.Limit)]
		var last eventstore.AnalyticsLandingCursor
		for _, row := range candidates {
			last = eventstore.AnalyticsLandingCursor{At: row.LandedAt.UTC(), ChangeID: row.ID}
			if row.Eligible {
				landing, err := eventstore.VersionLandingFromChange(eventstore.Change(row.ChangeRequest))
				if err != nil {
					return out, err
				}
				out.Items = append(out.Items, landing)
			}
		}
		if more {
			out.Next = last
			out.Partial = true
		}
		return out, nil
	}
	args = []any{s.organization, q.ProjectID, q.To.UTC(), q.Limit + 1, q.Principal}
	if q.After.ChangeID != "" {
		seek = " AND (v.landed_at,v.change_id,v.id)>($6,$7,$8)"
		args = append(args, q.After.At.UTC(), q.After.ChangeID, q.After.VersionID)
	}
	rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED (
 SELECT v.* FROM change_versions v WHERE v.organization_id=$1 AND v.project_id=$2 AND v.landed_at IS NOT NULL AND v.landed_at<$3`+seek+` ORDER BY v.landed_at,v.change_id,v.id LIMIT $4
 ),change_metadata AS MATERIALIZED(
 SELECT requested.change_id,c.work_item_id FROM (SELECT DISTINCT change_id FROM examined)requested LEFT JOIN LATERAL(SELECT c.work_item_id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=$2 AND c.id=requested.change_id LIMIT 1)c ON true
 ),item_metadata AS MATERIALIZED(
 SELECT requested.work_item_id,i.native_id,i.actor_principal_id FROM (SELECT DISTINCT work_item_id FROM change_metadata)requested LEFT JOIN LATERAL(SELECT i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=requested.work_item_id LIMIT 1)i ON true
 ) SELECT v.*,coalesce(c.work_item_id,'') AS item_id,i.native_id IS NOT NULL AND ($5='' OR coalesce(i.actor_principal_id,'')=$5) AS eligible
 FROM examined v LEFT JOIN change_metadata c ON c.change_id=v.change_id LEFT JOIN item_metadata i ON i.work_item_id=c.work_item_id
 ORDER BY v.landed_at,v.change_id,v.id`, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type candidate struct {
		pgquery.ChangeVersion
		ItemID   tracker.NativeWorkItemID
		Eligible bool
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
	if err != nil {
		return out, mapError(ctx, err)
	}
	reopened := map[tracker.NativeWorkItemID]bool{}
	for _, item := range q.ReopenedItems {
		reopened[item] = true
	}
	escaped := map[string]bool{}
	for _, version := range q.EscapedVersions {
		escaped[version] = true
	}
	more := len(candidates) > q.Limit
	candidates = candidates[:min(len(candidates), q.Limit)]
	var last eventstore.AnalyticsLandingCursor
	for _, row := range candidates {
		last = eventstore.AnalyticsLandingCursor{At: row.LandedAt.UTC(), ChangeID: row.ChangeID, VersionID: row.ID}
		if !row.Eligible || row.LandedAt.Before(q.From) && !reopened[row.ItemID] && !escaped[row.ChangeID+"/"+row.ID] {
			continue
		}
		landing := versionRecord(row.ChangeVersion).FrozenLanding
		if landing == nil {
			return out, eventstore.ErrInvalidRecord
		}
		landing.WorkItemID = row.ItemID
		out.Items = append(out.Items, *landing)
	}
	if more {
		out.Next = last
		out.Partial = true
	}
	var unknown, authorized int
	err = s.db.QueryRow(ctx, `WITH examined AS MATERIALIZED(SELECT v.change_id,v.created_at FROM change_versions v WHERE v.organization_id=$1 AND v.project_id=$2 AND NOT v.landing_history_known ORDER BY v.id LIMIT $3),change_metadata AS MATERIALIZED(
 SELECT requested.change_id,c.work_item_id FROM (SELECT DISTINCT change_id FROM examined)requested LEFT JOIN LATERAL(SELECT c.work_item_id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=$2 AND c.id=requested.change_id LIMIT 1)c ON true
 ),item_metadata AS MATERIALIZED(
 SELECT requested.work_item_id,i.native_id,i.actor_principal_id FROM (SELECT DISTINCT work_item_id FROM change_metadata)requested LEFT JOIN LATERAL(SELECT i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=requested.work_item_id LIMIT 1)i ON true
 ) SELECT count(*),count(*) FILTER(WHERE v.created_at<$4 AND i.native_id IS NOT NULL AND ($5='' OR coalesce(i.actor_principal_id,'')=$5)) FROM examined v LEFT JOIN change_metadata c ON c.change_id=v.change_id LEFT JOIN item_metadata i ON i.work_item_id=c.work_item_id`, s.organization, q.ProjectID, q.Limit+1, q.To.UTC(), q.Principal).Scan(&unknown, &authorized)
	if err != nil {
		return out, mapError(ctx, err)
	}
	out.HistoryUnknown = authorized > 0 || unknown > q.Limit
	out.Partial = out.Partial || out.HistoryUnknown
	return out, nil
}

func (s *session) ReadAnalyticsEvidencePage(ctx context.Context, q eventstore.AnalyticsEvidenceQuery) (eventstore.AnalyticsEvidencePage, error) {
	out := eventstore.AnalyticsEvidencePage{Items: []eventstore.AnalyticsEvidenceRow{}, Partial: q.After.ChangeID != ""}
	pending := q.After.VersionThrough != 0
	if q.ProjectID == "" || q.Limit < 1 || q.Limit > 1000 || !q.From.Before(q.To) || q.After.Sequence < 0 || (q.After.ChangeID == "") != q.After.At.IsZero() || q.After.At.Nanosecond()%1000 != 0 || (q.After.ChangeID == "") != (q.After.Sequence == 0) || q.After.VersionAfter < 0 || q.After.VersionThrough < q.After.VersionAfter || pending && q.After.ChangeID == "" || !pending && q.After.VersionAfter != 0 {
		return out, corestore.ErrInvalidPage
	}
	rawLimit := q.Limit + 1
	comparison := ">"
	if pending {
		rawLimit = 1
		comparison = ">="
	}
	args := []any{s.organization, q.ProjectID, q.From.UTC(), q.To.UTC(), rawLimit, q.Principal}
	predicate := `e.organization_id=$1 AND e.project_id=$2 AND e.quality_at IS NOT NULL AND e.quality_at>=$3 AND e.quality_at<$4`
	source := `WITH examined AS MATERIALIZED(SELECT e.* FROM change_evidence e WHERE ` + predicate + ` ORDER BY e.quality_at DESC,e.sequence,e.change_id LIMIT $5`
	if q.After.ChangeID != "" {
		args = append(args, q.After.At.UTC(), q.After.Sequence, q.After.ChangeID)
		source = `WITH equal_time AS MATERIALIZED(SELECT e.* FROM change_evidence e WHERE ` + predicate + ` AND e.quality_at=$7 AND (e.sequence,e.change_id)` + comparison + `($8,$9) ORDER BY e.sequence,e.change_id LIMIT $5),
 older_time AS MATERIALIZED(SELECT e.* FROM change_evidence e WHERE ` + predicate + ` AND e.quality_at<$7 ORDER BY e.quality_at DESC,e.sequence,e.change_id LIMIT (SELECT $5-count(*) FROM equal_time)),
 examined AS MATERIALIZED(SELECT * FROM equal_time UNION ALL SELECT * FROM older_time ORDER BY quality_at DESC,sequence,change_id LIMIT $5`
	}
	rows, err := s.db.Query(ctx, source+` ),change_metadata AS MATERIALIZED(
 SELECT requested.change_id,c.work_item_id FROM (SELECT DISTINCT change_id FROM examined)requested LEFT JOIN LATERAL(SELECT c.work_item_id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=$2 AND c.id=requested.change_id LIMIT 1)c ON true
 ),item_metadata AS MATERIALIZED(
 SELECT requested.work_item_id,i.native_id,i.actor_principal_id FROM (SELECT DISTINCT work_item_id FROM change_metadata)requested LEFT JOIN LATERAL(SELECT i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=requested.work_item_id LIMIT 1)i ON true
 ),version_metadata AS MATERIALIZED(
 SELECT requested.change_id,requested.version_id,coalesce(current_version.number,0) AS current_number,next_version.number AS next_number,next_version.created_at AS next_created_at,coalesce(last_version.number,0) AS last_number
 FROM (SELECT DISTINCT change_id,version_id FROM examined WHERE kind='review' AND decision='changes_requested' AND created_at>=$3 AND created_at<$4)requested
 LEFT JOIN LATERAL(SELECT v.number FROM change_versions v WHERE v.organization_id=$1 AND v.change_id=requested.change_id AND v.id=requested.version_id LIMIT 1)current_version ON true
 LEFT JOIN LATERAL(SELECT v.number,v.created_at FROM change_versions v WHERE v.organization_id=$1 AND v.change_id=requested.change_id AND v.number>current_version.number ORDER BY v.number LIMIT 1)next_version ON true
 LEFT JOIN LATERAL(SELECT v.number FROM change_versions v WHERE v.organization_id=$1 AND v.change_id=requested.change_id ORDER BY v.number DESC LIMIT 1)last_version ON true
 ) SELECT e.*,coalesce(c.work_item_id,'') AS item_id,i.native_id IS NOT NULL AND ($6='' OR coalesce(i.actor_principal_id,'')=$6) AS eligible,
 coalesce(v.current_number,0) AS current_number,v.next_number,v.next_created_at,coalesce(v.last_number,0) AS last_number,
 coalesce(f.occurrence_id,'') AS escape_occurrence_id,coalesce(f.kind,'') AS escape_kind,coalesce(f.cause,'') AS escape_cause,f.observed_at AS escape_observed_at,coalesce(f.instance_id,'') AS escape_instance_id,coalesce(f.human_override,false) AS escape_human_override
 FROM examined e LEFT JOIN change_metadata c ON c.change_id=e.change_id LEFT JOIN item_metadata i ON i.work_item_id=c.work_item_id
 LEFT JOIN version_metadata v ON v.change_id=e.change_id AND v.version_id IS NOT DISTINCT FROM e.version_id
 LEFT JOIN report_escape_facts f ON f.organization_id=$1 AND f.change_id=e.change_id AND f.evidence_sequence=e.sequence
 ORDER BY e.quality_at DESC,e.sequence,e.change_id`, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type candidate struct {
		pgquery.ChangeEvidence
		ItemID                                      tracker.NativeWorkItemID
		Eligible                                    bool
		CurrentNumber                               int64
		NextNumber                                  *int64
		NextCreatedAt                               *time.Time
		LastNumber                                  int64
		EscapeOccurrenceID, EscapeKind, EscapeCause string
		EscapeObservedAt                            *time.Time
		EscapeInstanceID                            string
		EscapeHumanOverride                         bool
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
	if err != nil {
		return out, mapError(ctx, err)
	}
	more := len(candidates) > q.Limit
	candidates = candidates[:min(len(candidates), q.Limit)]
	if pending && (len(candidates) != 1 || candidates[0].QualityAt == nil || !candidates[0].QualityAt.Equal(q.After.At) || candidates[0].ChangeID != q.After.ChangeID || candidates[0].Sequence != q.After.Sequence || candidates[0].LastNumber != q.After.VersionThrough) {
		return out, corestore.ErrRevisionConflict
	}
	remainingVersions := q.Limit
	var last eventstore.AnalyticsEvidenceCursor
	for _, row := range candidates {
		if row.QualityAt == nil {
			return out, eventstore.ErrInvalidRecord
		}
		last = eventstore.AnalyticsEvidenceCursor{At: row.QualityAt.UTC(), Sequence: row.Sequence, ChangeID: row.ChangeID}
		if !row.Eligible {
			continue
		}
		eligible := false
		var escape *reportstore.EscapeProjection
		if row.Kind == "review" && row.Decision == "changes_requested" && !row.CreatedAt.Before(q.From) && row.CreatedAt.Before(q.To) {
			if !row.QualityAt.Equal(row.CreatedAt) {
				return out, eventstore.ErrInvalidRecord
			}
			eligible = true
			if pending || row.NextNumber != nil {
				through, after := row.LastNumber, row.CurrentNumber
				if pending {
					through, after = q.After.VersionThrough, q.After.VersionAfter
				} else {
					if row.NextCreatedAt == nil {
						return out, eventstore.ErrInvalidRecord
					}
					if !row.NextCreatedAt.After(row.CreatedAt) {
						eligible = false
					} else {
						after = *row.NextNumber
					}
				}
				if eligible && after < through {
					versions := []pgquery.ChangeVersion{}
					if remainingVersions > 0 {
						versions, err = s.queries.RecordListVersion(ctx, pgquery.RecordListVersionParams{OrganizationID: s.organization, ChangeID: row.ChangeID, After: after, PageLimit: int32(remainingVersions)})
						if err != nil {
							return out, mapError(ctx, err)
						}
						remainingVersions -= len(versions)
					}
					for _, version := range versions {
						if version.Number > through {
							break
						}
						after = version.Number
						if !version.CreatedAt.After(row.CreatedAt) {
							eligible = false
							break
						}
					}
					if eligible && after < through {
						out.Next = eventstore.AnalyticsEvidenceCursor{At: row.QualityAt.UTC(), Sequence: row.Sequence, ChangeID: row.ChangeID, VersionAfter: after, VersionThrough: through}
						out.Partial = true
						return out, nil
					}
				}
			}
		} else if row.Kind == "discussion" && row.EscapeOccurrenceID != "" && row.EscapeObservedAt != nil && !row.EscapeObservedAt.Before(q.From) && row.EscapeObservedAt.Before(q.To) {
			if !row.QualityAt.Equal(*row.EscapeObservedAt) {
				return out, eventstore.ErrInvalidRecord
			}
			eligible = row.CreatedAt.Before(q.To)
			escape = &reportstore.EscapeProjection{OccurrenceID: row.EscapeOccurrenceID, Kind: row.EscapeKind, Cause: row.EscapeCause, ObservedAt: row.EscapeObservedAt.UTC(), InstanceID: row.EscapeInstanceID, HumanOverride: row.EscapeHumanOverride}
		}
		if eligible {
			out.Items = append(out.Items, eventstore.AnalyticsEvidenceRow{WorkItemID: row.ItemID, Evidence: evidenceRecord(row.ChangeEvidence), Escape: escape})
		}
	}
	if pending {
		var newer bool
		err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM change_evidence e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.quality_at=$3 AND e.quality_at>=$4 AND e.quality_at<$5 AND (e.sequence,e.change_id)>($6,$7) ORDER BY e.sequence,e.change_id LIMIT 1) OR EXISTS(SELECT 1 FROM change_evidence e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.quality_at>=$4 AND e.quality_at<$3 ORDER BY e.quality_at DESC,e.sequence,e.change_id LIMIT 1)`, s.organization, q.ProjectID, last.At, q.From.UTC(), q.To.UTC(), last.Sequence, last.ChangeID).Scan(&newer)
		if err != nil {
			return out, mapError(ctx, err)
		}
		more = newer
	}
	if more {
		out.Next = last
		out.Partial = true
	}
	return out, nil
}

func (s *session) QualityEscapeOccurrence(ctx context.Context, change, version, occurrence string) (eventstore.Evidence, error) {
	if change == "" || version == "" || occurrence == "" {
		return eventstore.Evidence{}, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `WITH examined AS MATERIALIZED(SELECT f.organization_id,f.change_id,f.evidence_sequence FROM report_escape_facts f WHERE f.organization_id=$1 AND f.change_id=$2 AND f.occurrence_id=$3 ORDER BY f.evidence_sequence DESC LIMIT 1001) SELECT e.organization_id,e.change_id,e.sequence,e.version_id,e.kind,e.source_key,e.actor_kind,e.actor_principal_id,e.decision,e.created_at,e.payload_object_key,e.payload_sha256,e.payload_bytes,e.payload_content_bytes,e.payload_format_version,e.review_kind,e.review_landing,e.review_session_known,e.review_session_id,e.review_session_machine FROM examined f JOIN change_evidence e ON e.organization_id=f.organization_id AND e.change_id=f.change_id AND e.sequence=f.evidence_sequence ORDER BY f.evidence_sequence DESC`, s.organization, change, occurrence)
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var out eventstore.Evidence
		if err := rows.Scan(&out.OrganizationID, &out.ChangeID, &out.Sequence, &out.VersionID, &out.Kind, &out.SourceKey, &out.ActorKind, &out.ActorPrincipalID, &out.Decision, &out.CreatedAt, &out.PayloadObjectKey, &out.PayloadSHA256, &out.PayloadBytes, &out.PayloadContentBytes, &out.PayloadFormatVersion, &out.ReviewKind, &out.ReviewLanding, &out.ReviewSessionKnown, &out.ReviewSessionID, &out.ReviewSessionMachine); err != nil {
			return eventstore.Evidence{}, mapError(ctx, err)
		}
		count++
		if count > 1000 {
			return eventstore.Evidence{}, reportstore.ErrPopulationExceeded
		}
		if out.VersionID != nil && *out.VersionID == version {
			return out, nil
		}
	}
	if err := rows.Err(); err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	return eventstore.Evidence{}, corestore.ErrNotFound
}
