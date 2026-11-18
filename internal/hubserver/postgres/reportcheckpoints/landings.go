package reportcheckpoints

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportreads"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const StaleLandings = 16

const exactLandings = `SELECT i.project_id,i.native_id,c.id,c.landed_at,coalesce(tries.n,0),coalesce(tries.complete,false),c.landed_version_id,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref,coalesce(tries.observed,0)
FROM unnest($15::text[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM change_requests c WHERE c.organization_id=$1 AND c.id=candidate.id LIMIT 1) c
CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id LIMIT 1) i
JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id
LEFT JOIN LATERAL (
 WITH candidate_attempts AS MATERIALIZED (
   SELECT a.id,a.started_at FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=c.landed_at AND c.landed_at>=$4 ORDER BY a.started_at,a.id LIMIT $11
 ), attempts AS (
   SELECT a.id,a.started_at,f.landing_receipt,f.landed,f.landing_change_id
   FROM candidate_attempts a
   LEFT JOIN LATERAL (SELECT rf.* FROM collaboration_events e JOIN report_event_facts rf ON rf.organization_id=e.organization_id AND rf.event_id=e.id WHERE e.organization_id=i.organization_id AND e.attempt_id=a.id AND e.attempt_id<>'' ORDER BY e.sequence DESC LIMIT 1) f ON true
   WHERE f.role='merge' OR f.landing_receipt OR f.event_id IS NULL
 ), numbered AS (SELECT *,row_number() OVER(ORDER BY started_at,id) n FROM attempts), success AS (
   SELECT min(n) n FROM numbered WHERE landed AND landing_change_id=c.id
 )
 SELECT success.n,success.n IS NOT NULL AND NOT EXISTS(SELECT 1 FROM numbered a WHERE a.n<=success.n AND NOT coalesce(a.landing_receipt,false)) AS complete,(SELECT count(*) FROM candidate_attempts) observed FROM success
) tries ON true
WHERE ` + reportreads.IssueFilter + ` AND c.landed_at<$5 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
ORDER BY c.landed_at DESC,c.id LIMIT $11`

const projectedLandings = `SELECT i.project_id,i.native_id,c.id,c.landed_at,c.landed_version_id,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref,
 t.change_id IS NOT NULL,t.landed_at,coalesce(t.attempts_observed,0),coalesce(t.success_position,0),coalesce(t.success_attempts,0),coalesce(t.success_complete,false),t.last_attempt_started_at,coalesce(t.last_attempt_id,''),
 coalesce(k.revision,0),coalesce(k.landing_revision,0),last.started_at,coalesce(last.id,'')
FROM unnest($15::text[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM change_requests c WHERE c.organization_id=$1 AND c.id=candidate.id LIMIT 1) c
CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id LIMIT 1) i
JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id
LEFT JOIN LATERAL (SELECT * FROM report_landing_attempts t WHERE t.organization_id=c.organization_id AND t.change_id=c.id LIMIT 1) t ON true
LEFT JOIN LATERAL (SELECT * FROM report_issue_checkpoints k WHERE k.organization_id=c.organization_id AND k.project_id=c.project_id AND k.work_item_id=c.work_item_id LIMIT 1) k ON true
LEFT JOIN LATERAL (SELECT a.started_at,a.id FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=c.landed_at ORDER BY a.started_at DESC,a.started_at_submicro_ns DESC,a.id DESC LIMIT 1) last ON true
WHERE ` + reportreads.IssueFilter + ` AND c.landed_at<$5 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
ORDER BY c.landed_at DESC,c.id LIMIT $11`

func LandingFacts(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, args []any, limit int, from time.Time) ([]reportstore.LandingFact, error) {
	projects, ok := args[1].([]string)
	if !ok || len(args) != 15 {
		return nil, errors.New("report landing arguments have an unexpected shape")
	}
	rows, err := db.Query(ctx, projectedLandings, args...)
	if err != nil {
		return nil, fmt.Errorf("read projected report landings: %w", err)
	}
	type candidate struct {
		fact                 reportstore.LandingFact
		landing              Landing
		revision, landingRev int64
		lastStarted          pgtype.Timestamptz
		lastID               string
	}
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		var stored pgtype.Timestamptz
		if err := rows.Scan(&c.fact.ProjectID, &c.fact.WorkItemID, &c.fact.ChangeID, &c.fact.At, &c.fact.VersionID, &c.fact.HeadSHA, &c.fact.MergeSHA, &c.fact.BaseRef,
			&c.landing.Present, &stored, &c.landing.Observed, &c.landing.SuccessPosition, &c.landing.SuccessAttempts, &c.landing.Complete, &c.landing.LastStartedAt, &c.landing.LastID,
			&c.revision, &c.landingRev, &c.lastStarted, &c.lastID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan projected report landing: %w", err)
		}
		c.landing.LandedAt = stored.Time
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read projected report landings: %w", err)
	}
	if len(candidates) > limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	lags, err := ReadLag(ctx, db, organization, projects)
	if err != nil {
		return nil, err
	}
	out := make([]reportstore.LandingFact, len(candidates))
	stale := map[string]int{}
	staleIDs := []string{}
	for i, c := range candidates {
		var lastStarted *time.Time
		if c.lastStarted.Valid {
			lastStarted = &c.lastStarted.Time
		}
		out[i] = c.fact
		if lags[string(c.fact.ProjectID)].Stale(string(c.fact.WorkItemID)) || !c.landing.Current(c.fact.At, c.revision, c.landingRev, lastStarted, c.lastID) {
			stale[c.fact.ChangeID] = i
			staleIDs = append(staleIDs, c.fact.ChangeID)
			continue
		}
		var observed int
		out[i].Attempts, out[i].Complete, observed = c.landing.Tries(limit, c.fact.At, from)
		if observed > limit && !out[i].Complete {
			return nil, reportstore.ErrPopulationExceeded
		}
	}
	if len(staleIDs) == 0 || len(staleIDs) > StaleLandings {
		return out, nil
	}
	exactArgs := append(append([]any{}, args[:14]...), staleIDs)
	rows, err = db.Query(ctx, exactLandings, exactArgs...)
	if err != nil {
		return nil, fmt.Errorf("read exact report landings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fact reportstore.LandingFact
		var observed int
		if err := rows.Scan(&fact.ProjectID, &fact.WorkItemID, &fact.ChangeID, &fact.At, &fact.Attempts, &fact.Complete, &fact.VersionID, &fact.HeadSHA, &fact.MergeSHA, &fact.BaseRef, &observed); err != nil {
			return nil, fmt.Errorf("scan exact report landing: %w", err)
		}
		if observed > limit && !fact.Complete {
			return nil, reportstore.ErrPopulationExceeded
		}
		if i, ok := stale[fact.ChangeID]; ok {
			out[i].Attempts, out[i].Complete = fact.Attempts, fact.Complete
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read exact report landings: %w", err)
	}
	return out, nil
}
