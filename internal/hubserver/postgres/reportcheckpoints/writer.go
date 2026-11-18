package reportcheckpoints

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	FeedBatch   = 256
	LandingPage = 16
	AttemptRows = 5001
)

type Cursor struct {
	Project     string
	WorkItem    string
	Revision    int64
	AfterChange string
}

const foldEvents = `entries AS (
 INSERT INTO report_issue_state_entries(organization_id,project_id,work_item_id,state,first_entered_at)
 SELECT $1,$2,work_item_id,to_state,min(recorded_at) FROM batch WHERE type='workflow.transitioned' AND to_state<>'' GROUP BY work_item_id,to_state
 ON CONFLICT (organization_id,project_id,work_item_id,state) DO UPDATE SET first_entered_at=EXCLUDED.first_entered_at
 WHERE EXCLUDED.first_entered_at<report_issue_state_entries.first_entered_at
), items AS (
 INSERT INTO report_issue_checkpoints(organization_id,project_id,work_item_id,revision)
 SELECT $1,$2,work_item_id,1 FROM batch GROUP BY work_item_id
 ON CONFLICT (organization_id,project_id,work_item_id) DO UPDATE SET revision=report_issue_checkpoints.revision+1
)`

func Tries(issue, change string, limit int) string {
	return fmt.Sprintf(`WITH candidate_attempts AS MATERIALIZED (
 SELECT a.id,a.started_at,row_number() OVER (ORDER BY a.started_at,a.id) pos FROM (SELECT a.id,a.started_at FROM native_attempts a WHERE a.organization_id=%[1]s.organization_id AND a.issue_id=%[1]s.id AND a.started_at<=%[2]s.landed_at ORDER BY a.started_at,a.id LIMIT %[3]d) a
), attempts AS (
 SELECT a.id,a.started_at,a.pos,f.landing_receipt,f.landed,f.landing_change_id
 FROM candidate_attempts a
 LEFT JOIN LATERAL (SELECT rf.* FROM collaboration_events e JOIN report_event_facts rf ON rf.organization_id=e.organization_id AND rf.event_id=e.id WHERE e.organization_id=%[1]s.organization_id AND e.attempt_id=a.id AND e.attempt_id<>'' ORDER BY e.sequence DESC LIMIT 1) f ON true
 WHERE f.role='merge' OR f.landing_receipt OR f.event_id IS NULL
), numbered AS (SELECT *,row_number() OVER (ORDER BY started_at,id) n FROM attempts), success AS (
 SELECT n,pos FROM numbered WHERE landed AND landing_change_id=%[2]s.id ORDER BY n LIMIT 1
)
SELECT (SELECT count(*) FROM candidate_attempts)::integer AS observed,success.pos::integer AS success_position,success.n::integer AS success_attempts,
 coalesce(success.n IS NOT NULL AND NOT EXISTS (SELECT 1 FROM numbered a WHERE a.n<=success.n AND NOT coalesce(a.landing_receipt,false)),false) AS success_complete
FROM (SELECT 1) one LEFT JOIN success ON true`, issue, change, limit)
}

func Step(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, cursor Cursor) (Cursor, bool, error) {
	if organization == "" {
		return cursor, false, errors.New("report checkpoints require an organization")
	}
	if cursor.WorkItem != "" {
		return landingPage(ctx, db, organization, cursor)
	}
	var project string
	err := db.QueryRow(ctx, `SELECT p.id FROM projects p
LEFT JOIN project_event_cursors h ON h.organization_id=p.organization_id AND h.project_id=p.id
LEFT JOIN report_checkpoint_cursors c ON c.organization_id=p.organization_id AND c.project_id=p.id
WHERE p.organization_id=$1 AND (c.project_id IS NULL OR NOT c.legacy_complete OR coalesce(h.sequence,0)>c.through_project_sequence
 OR EXISTS (SELECT 1 FROM report_issue_checkpoints k WHERE k.organization_id=p.organization_id AND k.project_id=p.id AND k.landing_revision<k.revision)
 OR EXISTS (SELECT 1 FROM report_issue_checkpoints k WHERE k.organization_id=p.organization_id AND k.project_id=p.id AND `+historyDue+`)
 OR EXISTS (SELECT 1 FROM report_issue_checkpoints k WHERE k.organization_id=p.organization_id AND k.project_id=p.id AND k.history_through>0 AND k.history_policy_revision<c.history_policy_revision))
ORDER BY p.id>$2 DESC,p.id LIMIT 1`, organization, cursor.Project).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return cursor, false, fmt.Errorf("select report checkpoint project: %w", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO report_checkpoint_cursors(organization_id,project_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, organization, project); err != nil {
		return cursor, false, fmt.Errorf("create report checkpoint cursor: %w", err)
	}
	var through, head, legacySequence int64
	var legacyItem string
	var legacyComplete bool
	err = db.QueryRow(ctx, `SELECT c.through_project_sequence,coalesce((SELECT h.sequence FROM project_event_cursors h WHERE h.organization_id=c.organization_id AND h.project_id=c.project_id),0),c.legacy_work_item_id,c.legacy_sequence,c.legacy_complete
FROM report_checkpoint_cursors c WHERE c.organization_id=$1 AND c.project_id=$2 FOR UPDATE`, organization, project).Scan(&through, &head, &legacyItem, &legacySequence, &legacyComplete)
	if err != nil {
		return cursor, false, fmt.Errorf("lock report checkpoint cursor: %w", err)
	}
	next := Cursor{Project: project}
	switch {
	case !legacyComplete:
		_, err = db.Exec(ctx, `WITH batch AS MATERIALIZED (
 SELECT e.work_item_id,e.sequence,e.type,e.to_state,e.recorded_at FROM collaboration_events e
 WHERE e.organization_id=$1 AND e.project_id=$2 AND e.project_sequence=0 AND e.work_item_id>=$3 AND (e.work_item_id,e.sequence)>($3::text,$4::bigint)
 ORDER BY e.project_sequence,e.work_item_id,e.sequence LIMIT $5
), last AS (SELECT work_item_id,sequence FROM batch ORDER BY work_item_id DESC,sequence DESC LIMIT 1), `+foldEvents+`
UPDATE report_checkpoint_cursors SET legacy_work_item_id=coalesce((SELECT work_item_id FROM last),legacy_work_item_id),legacy_sequence=coalesce((SELECT sequence FROM last),legacy_sequence),legacy_complete=(SELECT count(*) FROM batch)<$5
WHERE organization_id=$1 AND project_id=$2`, organization, project, legacyItem, legacySequence, FeedBatch)
		if err != nil {
			return cursor, false, fmt.Errorf("fold legacy report checkpoint events: %w", err)
		}
		return next, true, nil
	case head > through:
		_, err = db.Exec(ctx, `WITH bound AS (
 SELECT max(project_sequence) AS through FROM (SELECT e.project_sequence FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.project_sequence>$3 ORDER BY e.project_sequence LIMIT $4) page
), batch AS MATERIALIZED (
 SELECT e.work_item_id,e.type,e.to_state,e.recorded_at FROM collaboration_events e
 WHERE e.organization_id=$1 AND e.project_id=$2 AND e.project_sequence>$3 AND e.project_sequence<=(SELECT through FROM bound)
), `+foldEvents+`
UPDATE report_checkpoint_cursors SET through_project_sequence=coalesce((SELECT through FROM bound),$5) WHERE organization_id=$1 AND project_id=$2`, organization, project, through, FeedBatch, head)
		if err != nil {
			return cursor, false, fmt.Errorf("fold report checkpoint events: %w", err)
		}
		return next, true, nil
	}
	err = db.QueryRow(ctx, `SELECT work_item_id,revision FROM report_issue_checkpoints WHERE organization_id=$1 AND project_id=$2 AND landing_revision<revision ORDER BY work_item_id LIMIT 1`, organization, project).Scan(&next.WorkItem, &next.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return next, true, historyStep(ctx, db, organization, project)
	}
	if err != nil {
		return cursor, false, fmt.Errorf("select due report landing checkpoint: %w", err)
	}
	return landingPage(ctx, db, organization, next)
}

func landingPage(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, cursor Cursor) (Cursor, bool, error) {
	var last string
	var count int
	err := db.QueryRow(ctx, `WITH changes AS MATERIALIZED (
 SELECT c.id,c.landed_at FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=$2 AND c.work_item_id=$3 AND c.landed_at IS NOT NULL AND c.id>$4 ORDER BY c.id LIMIT $5
), computed AS (
 SELECT c.id,c.landed_at,t.observed,t.success_position,t.success_attempts,t.success_complete,last.started_at,coalesce(last.id,'') AS last_id
 FROM changes c CROSS JOIN LATERAL (SELECT i.organization_id,i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 LIMIT 1) i
 CROSS JOIN LATERAL (`+Tries("i", "c", AttemptRows)+`) t
 LEFT JOIN LATERAL (SELECT a.started_at,a.id FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=c.landed_at ORDER BY a.started_at DESC,a.started_at_submicro_ns DESC,a.id DESC LIMIT 1) last ON true
), stored AS (
 INSERT INTO report_landing_attempts(organization_id,change_id,landed_at,attempts_observed,success_position,success_attempts,success_complete,last_attempt_started_at,last_attempt_id)
 SELECT $1,id,landed_at,observed,success_position,success_attempts,success_complete,started_at,last_id FROM computed
 ON CONFLICT (organization_id,change_id) DO UPDATE SET landed_at=EXCLUDED.landed_at,attempts_observed=EXCLUDED.attempts_observed,success_position=EXCLUDED.success_position,success_attempts=EXCLUDED.success_attempts,success_complete=EXCLUDED.success_complete,last_attempt_started_at=EXCLUDED.last_attempt_started_at,last_attempt_id=EXCLUDED.last_attempt_id
)
SELECT coalesce(max(id),''),count(*) FROM changes`, organization, cursor.Project, cursor.WorkItem, cursor.AfterChange, LandingPage).Scan(&last, &count)
	if err != nil {
		return cursor, false, fmt.Errorf("refresh report landing checkpoint: %w", err)
	}
	if count == LandingPage {
		cursor.AfterChange = last
		return cursor, true, nil
	}
	if _, err := db.Exec(ctx, `UPDATE report_issue_checkpoints SET landing_revision=$4 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND landing_revision<$4 AND revision>=$4`, organization, cursor.Project, cursor.WorkItem, cursor.Revision); err != nil {
		return cursor, false, fmt.Errorf("settle report landing checkpoint: %w", err)
	}
	return Cursor{Project: cursor.Project}, true, nil
}
