package reportreads

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	CandidateRows = 50000
	maxProjects   = 1000
	windowPage    = 500
	maxStateNames = 1000
)

const IssueFilter = `i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND ($3='' OR i.actor_principal_id=$3)
AND (coalesce(cardinality($6::text[]),0)=0 OR i.issue_type=ANY($6::text[]))
AND (coalesce(cardinality($7::integer[]),0)=0 OR i.priority=ANY($7::integer[]))
AND (coalesce(cardinality($8::text[]),0)=0 OR i.actor_principal_id=ANY($8::text[]))
AND (coalesce(cardinality($9::text[]),0)=0 OR EXISTS (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY($9::text[])))
AND (coalesce(cardinality($10::text[]),0)=0 OR EXISTS (SELECT 1 FROM native_attempts a JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<$5 AND a.updated_at>=$4 AND la.runner_id=ANY($10::text[])))
AND (coalesce(cardinality($12::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.recorded_at>=$4 AND e.recorded_at<$5 AND coalesce(nullif(trim(f.model),''),'Unavailable')=ANY($12::text[])))
AND (coalesce(cardinality($13::text[]),0)=0 OR EXISTS (SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.recorded_at>=$4 AND e.recorded_at<$5 AND coalesce(nullif(trim(f.role),''),'Unavailable')=ANY($13::text[])))`

func FactPages(name, source, order string, limit int) string {
	first := min(limit+1, CandidateRows)
	firstName, secondName := name+"_first", name+"_second"
	firstPage := firstName + " AS MATERIALIZED (" + source + " ORDER BY " + order + " LIMIT " + strconv.Itoa(first) + ")"
	remaining := limit + 1 - first
	if remaining == 0 {
		return firstPage + ", " + name + " AS (SELECT * FROM " + firstName + ")"
	}
	reverse := strings.ReplaceAll(order, ",", " DESC,") + " DESC"
	return firstPage + ", " + secondName + " AS MATERIALIZED (" + source + " AND (SELECT count(*) FROM " + firstName + ")=" + strconv.Itoa(first) +
		" AND (" + order + ")>(SELECT " + order + " FROM " + firstName + " ORDER BY " + reverse + " LIMIT 1) ORDER BY " + order + " LIMIT " + strconv.Itoa(remaining) + ")," +
		" " + name + " AS (SELECT * FROM " + firstName + " UNION ALL SELECT * FROM " + secondName + ")"
}

func AttemptCandidates(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string, from, to time.Time, current bool) ([]string, error) {
	currentUsed, currentKeyed, currentExceeded := "", "", ""
	if current {
		currentUsed = `, used_current AS MATERIALIZED (
 SELECT u.attempt_id FROM report_usage_current u WHERE u.organization_id=$1 AND u.project_id=ANY($5::text[]) AND u.period>=$2 AND u.period<$3 ORDER BY u.project_id,u.period,u.provider,u.model,u.attempt_id,u.source_key LIMIT $4
)`
		currentKeyed = " UNION SELECT attempt_id FROM used_current"
		currentExceeded = " OR (SELECT count(*) FROM used_current)>=$4"
	}
	var exceeded bool
	var ids []string
	err := db.QueryRow(ctx, `WITH lease_page AS MATERIALIZED (
 SELECT l.lease_id,l.machine_id FROM leases l WHERE l.organization_id=$1 AND (l.project_id=ANY($5::text[]) OR l.project_id IS NULL) AND l.expires_at>=$2
 ORDER BY l.project_id,l.expires_at,l.lease_id LIMIT $4
), leased AS MATERIALIZED (
 SELECT a.id,l.machine_id FROM lease_page l CROSS JOIN LATERAL (
  SELECT a.id FROM native_attempts a WHERE a.organization_id=$1 AND a.lease_id=l.lease_id AND a.project_id=ANY($5::text[]) LIMIT 1
 ) a
), evented AS MATERIALIZED (
 SELECT e.attempt_id FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=ANY($5::text[]) AND e.recorded_at>=$2 AND e.recorded_at<$3 AND e.attempt_id<>'' ORDER BY e.project_id,e.recorded_at,e.id LIMIT $4
)`+currentUsed+`, keyed AS (
 SELECT id FROM leased UNION SELECT attempt_id FROM evented`+currentKeyed+`
)
SELECT (SELECT count(*) FROM lease_page)>=$4 OR (SELECT count(DISTINCT machine_id) FROM leased)>$6 OR (SELECT count(*) FROM evented)>=$4`+currentExceeded+`,
 coalesce((SELECT array_agg(id ORDER BY id) FROM keyed),'{}'::text[])`, organization, from, to, CandidateRows+1, projects, maxProjects).Scan(&exceeded, &ids)
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	if exceeded {
		return nil, reportstore.ErrPopulationExceeded
	}
	used, err := usageWindowAttempts(ctx, db, organization, projects, from, to)
	if err != nil {
		return nil, err
	}
	ids = append(ids, used...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > CandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	return ids, nil
}

func usageWindowAttempts(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string, from, to time.Time) ([]string, error) {
	ids := []string{}
	project, period, attempt := "", from, ""
	err := keyset(ctx, db, `SELECT u.project_id,u.period,u.attempt_id FROM attempt_usage u
WHERE u.organization_id=$1 AND u.project_id=ANY($2::text[]) AND u.period>=$3 AND u.period<$4 AND (u.project_id,u.period,u.attempt_id)>($6::text,$7::timestamptz,$8::text)
ORDER BY u.project_id,u.period,u.attempt_id LIMIT $5`, []any{organization, projects, from, to}, []any{&project, &period, &attempt}, func() { ids = append(ids, attempt) })
	return ids, err
}

func StartedWindowAttempts(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string, from, to time.Time) ([]string, error) {
	ids := []string{}
	project, started, attempt := "", from, ""
	var submicro int16
	err := keyset(ctx, db, `SELECT a.project_id,a.started_at,a.started_at_submicro_ns,a.id FROM native_attempts a
WHERE a.organization_id=$1 AND a.project_id=ANY($2::text[]) AND a.started_at>=$3 AND a.started_at<$4 AND (a.project_id,a.started_at,a.started_at_submicro_ns,a.id)>($6::text,$7::timestamptz,$8::smallint,$9::text)
ORDER BY a.project_id,a.started_at,a.started_at_submicro_ns,a.id LIMIT $5`, []any{organization, projects, from, to}, []any{&project, &started, &submicro, &attempt}, func() { ids = append(ids, attempt) })
	return ids, err
}

func UpdatedWindowIssues(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string, from time.Time) ([]int64, error) {
	ids := []int64{}
	project, updated := "", from
	var id int64
	err := keyset(ctx, db, `SELECT i.project_id,i.native_updated_at,i.id FROM issues i
WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND i.native_updated_at>=$3 AND (i.project_id,i.native_updated_at,i.id)>($5::text,$6::timestamptz,$7::bigint)
ORDER BY i.project_id,i.native_updated_at,i.id LIMIT $4`, []any{organization, projects, from}, []any{&project, &updated, &id}, func() { ids = append(ids, id) })
	return ids, err
}

func keyset(ctx context.Context, db pgquery.DBTX, query string, args, cursor []any, collect func()) error {
	count := 0
	for {
		page := min(windowPage, CandidateRows+1-count)
		rows, err := db.Query(ctx, query, append(append(slices.Clone(args), page), cursor...)...)
		if err != nil {
			return pgcore.MapError(ctx, err)
		}
		read := 0
		for rows.Next() {
			if err := rows.Scan(cursor...); err != nil {
				rows.Close()
				return pgcore.MapError(ctx, err)
			}
			collect()
			read++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return pgcore.MapError(ctx, err)
		}
		count += read
		if count > CandidateRows {
			return reportstore.ErrPopulationExceeded
		}
		if read < page {
			return nil
		}
	}
}

func UsageRows(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, from, to time.Time, limit int, attempts []reportstore.AttemptFact, current bool) ([]reportstore.UsageFact, error) {
	ids := make([]string, len(attempts))
	projects := make([]string, len(attempts))
	owners := make(map[string]*reportstore.AttemptFact, len(attempts))
	for i := range attempts {
		ids[i], projects[i] = attempts[i].AttemptID, string(attempts[i].ProjectID)
		owners[ids[i]] = &attempts[i]
	}
	page := usagePage{from: from, to: to}
	imported, err := page.read(ctx, db, organization, ids, limit)
	if err != nil {
		return nil, err
	}
	args := []any{organization, ids, projects}
	pages := imported.input("attempt_keys", false, &args)
	usage := `
 SELECT u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,'attempt' AS source_kind,u.usage_origin AS source_key
 FROM attempt_keys k CROSS JOIN LATERAL (SELECT u.* FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=k.attempt_id AND u.period=k.period AND u.provider=k.provider AND u.model=k.model AND u.currency=k.currency AND u.usage_origin=k.usage_origin LIMIT 1) u`
	if current {
		page.current = true
		keys, err := page.read(ctx, db, organization, ids, limit)
		if err != nil {
			return nil, err
		}
		pages += ", " + keys.input("current_keys", true, &args)
		usage += `
 WHERE coalesce((SELECT false FROM report_usage_current unknown WHERE unknown.organization_id=u.organization_id AND unknown.attempt_id=u.attempt_id AND unknown.provider=u.provider AND unknown.model=u.model AND unknown.usage_kind IS NULL LIMIT 1),true)
 UNION ALL
 SELECT u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,coalesce(u.currency,c.currency) AS currency,'current' AS source_kind,u.source_key AS source_key
 FROM current_keys k CROSS JOIN LATERAL (SELECT u.* FROM report_usage_current u WHERE u.organization_id=$1 AND u.source_key=k.source_key LIMIT 1) u
 CROSS JOIN LATERAL (SELECT c.currency FROM cost_sources c WHERE c.organization_id=u.organization_id AND c.source_key=u.source_key LIMIT 1) c
 WHERE u.usage_kind IS DISTINCT FROM 'cumulative' OR coalesce((SELECT false FROM attempt_usage active WHERE active.organization_id=u.organization_id AND active.attempt_id=u.attempt_id AND active.provider=u.provider AND active.model=u.model LIMIT 1),true)`
	}
	rows, err := db.Query(ctx, "WITH "+pages+`, usage AS MATERIALIZED (`+usage+`
)
SELECT u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,u.source_kind,u.source_key FROM usage u
WHERE upper(u.currency)='USD' AND (u.attempt_id,u.project_id) IN (SELECT s.attempt_id,s.project_id FROM ROWS FROM (unnest($2::text[]),unnest($3::text[])) AS s(attempt_id,project_id))
ORDER BY period,attempt_id,provider,model,source_kind,source_key,currency`, args...)
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	defer rows.Close()
	out := []reportstore.UsageFact{}
	for rows.Next() {
		var fact reportstore.UsageFact
		var kind, key string
		if err := rows.Scan(&fact.ProjectID, &fact.AttemptID, &fact.Period, &fact.Provider, &fact.Model, &fact.Input, &fact.CachedInput, &fact.Output, &fact.Cost, &fact.Currency, &kind, &key); err != nil {
			return nil, pgcore.MapError(ctx, err)
		}
		attempt := owners[fact.AttemptID]
		if attempt == nil || attempt.ProjectID != fact.ProjectID {
			return nil, corestore.ErrTenantMismatch
		}
		fact.WorkItemID, fact.RunnerID, fact.RunnerName = attempt.WorkItemID, attempt.RunnerID, attempt.RunnerName
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	if len(out) > limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}

func EscapeFacts(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, args []any, limit int) ([]reportstore.EscapeFact, error) {
	var known bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM report_escape_facts WHERE organization_id=$1)`, organization).Scan(&known); err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	if !known {
		return []reportstore.EscapeFact{}, nil
	}
	rows, err := db.Query(ctx, `WITH observed AS MATERIALIZED (
 SELECT f.change_id FROM report_escape_facts f WHERE f.organization_id=$1 AND f.observed_at>=$4 AND f.observed_at<$5 ORDER BY f.observed_at,f.change_id LIMIT $14
), landed AS MATERIALIZED (
 SELECT c.id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=ANY($2::text[]) AND c.landed_at>=$4 ORDER BY c.project_id,c.landed_at,c.id LIMIT $14
), keyed AS MATERIALIZED (
 SELECT f.change_id,f.evidence_sequence,f.observed_at,f.change_id IN (SELECT id FROM landed) AS landed FROM report_escape_facts f WHERE f.organization_id=$1 AND f.change_id=ANY(ARRAY(SELECT change_id FROM observed UNION SELECT id FROM landed)) ORDER BY f.change_id,f.evidence_sequence LIMIT $14
), escapes AS MATERIALIZED (
 SELECT f.*,c.project_id,c.work_item_id FROM keyed k
 CROSS JOIN LATERAL (SELECT f.* FROM report_escape_facts f WHERE f.organization_id=$1 AND f.change_id=k.change_id AND f.evidence_sequence=k.evidence_sequence ORDER BY f.change_id,f.evidence_sequence LIMIT 1) f
 CROSS JOIN LATERAL (SELECT c.project_id,c.work_item_id FROM change_requests c WHERE c.organization_id=$1 AND c.id=f.change_id LIMIT 1) c
 WHERE c.project_id=ANY($2::text[]) AND (k.observed_at>=$4 AND k.observed_at<$5 OR k.landed AND k.observed_at<$4)
)
SELECT false AS exceeded,i.project_id,i.native_id,f.change_id,coalesce(e.version_id,''),e.created_at,e.actor_kind,e.actor_principal_id,f.occurrence_id,f.kind,f.cause,f.observed_at,f.instance_id,f.human_override,e.sequence
FROM escapes f
CROSS JOIN LATERAL (SELECT e.version_id,e.created_at,e.actor_kind,e.actor_principal_id,e.sequence FROM change_evidence e WHERE e.organization_id=$1 AND e.change_id=f.change_id AND e.sequence=f.evidence_sequence ORDER BY e.change_id,e.sequence LIMIT 1) e
CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=$1 AND i.project_id=f.project_id AND i.native_id=f.work_item_id LIMIT 1) i
WHERE `+IssueFilter+` AND e.created_at<$5
UNION ALL
SELECT true,'','','','',$5::timestamptz,'','','','','',$5::timestamptz,'',false,0::bigint
WHERE (SELECT count(*) FROM observed)>=$14 OR (SELECT count(*) FROM landed)>=$14 OR (SELECT count(*) FROM keyed)>=$14
ORDER BY exceeded DESC,observed_at,created_at,sequence LIMIT $11`, append(args, CandidateRows+1)...)
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	defer rows.Close()
	out := []reportstore.EscapeFact{}
	for rows.Next() {
		var fact reportstore.EscapeFact
		var exceeded bool
		var sequence int64
		if err := rows.Scan(&exceeded, &fact.ProjectID, &fact.WorkItemID, &fact.ChangeID, &fact.VersionID, &fact.CreatedAt, &fact.ActorKind, &fact.ActorID, &fact.OccurrenceID, &fact.Kind, &fact.Cause, &fact.ObservedAt, &fact.InstanceID, &fact.HumanOverride, &sequence); err != nil {
			return nil, pgcore.MapError(ctx, err)
		}
		if exceeded {
			return nil, reportstore.ErrPopulationExceeded
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	if len(out) > limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}

func OpenCandidates(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string, at time.Time) ([]int64, error) {
	var exceeded bool
	var ids []int64
	err := db.QueryRow(ctx, `WITH RECURSIVE states AS (
 SELECT p.project_id,first.name,1 AS depth FROM unnest($2::text[]) p(project_id) CROSS JOIN LATERAL (
  SELECT i.workflow_state_name AS name FROM issues i WHERE i.organization_id=$1 AND i.project_id=p.project_id AND NOT i.archived AND i.workflow_state_name IS NOT NULL ORDER BY i.workflow_state_name,i.id LIMIT 1
 ) first
 UNION ALL
 SELECT s.project_id,next.name,s.depth+1 FROM states s CROSS JOIN LATERAL (
  SELECT i.workflow_state_name AS name FROM issues i WHERE i.organization_id=$1 AND i.project_id=s.project_id AND NOT i.archived AND i.workflow_state_name>s.name ORDER BY i.workflow_state_name,i.id LIMIT 1
 ) next WHERE s.depth<=$5
), open_states AS MATERIALIZED (
 SELECT s.project_id,s.name FROM states s LEFT JOIN project_workflow_states ws ON ws.organization_id=$1 AND ws.project_id=s.project_id AND ws.name=s.name
 WHERE NOT coalesce(ws.terminal,false)
), open AS MATERIALIZED (
 (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND NOT i.archived
  AND i.workflow_state_name=ANY(ARRAY(SELECT name FROM open_states))
  AND (cardinality($2::text[])=1 OR (i.project_id,i.workflow_state_name) IN (SELECT project_id,name FROM open_states))
  ORDER BY i.project_id,i.workflow_state_name,i.id LIMIT $4)
 UNION ALL
 (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND NOT i.archived AND i.workflow_state_name IS NULL ORDER BY i.project_id,i.id LIMIT $4)
), terminal AS MATERIALIZED (
 SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND NOT i.archived AND i.terminal_entered_at>$3 ORDER BY i.project_id,i.terminal_entered_at,i.id LIMIT $4
), updated AS MATERIALIZED (
 SELECT i.id,i.archived FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND i.native_updated_at>$3 ORDER BY i.project_id,i.native_updated_at,i.id LIMIT $4
), keyed AS (
 SELECT id FROM open
 UNION SELECT id FROM terminal
 UNION SELECT id FROM updated WHERE NOT archived
)
SELECT EXISTS(SELECT 1 FROM states WHERE depth>$5) OR (SELECT count(*) FROM open)>=$4 OR (SELECT count(*) FROM terminal)>=$4 OR (SELECT count(*) FROM updated)>=$4,
 coalesce((SELECT array_agg(id ORDER BY id) FROM keyed),'{}'::bigint[])`, organization, projects, at, CandidateRows+1, maxStateNames).Scan(&exceeded, &ids)
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	if exceeded || len(ids) > CandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	return ids, nil
}

func HistoryCandidates(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string, from, to time.Time) ([]int64, []string, error) {
	if len(projects) > maxProjects {
		return nil, nil, reportstore.ErrPopulationExceeded
	}
	var exceeded bool
	var ids []int64
	var active []string
	updated, err := UpdatedWindowIssues(ctx, db, organization, projects, from)
	if err != nil {
		return nil, nil, err
	}
	err = db.QueryRow(ctx, `WITH RECURSIVE states AS (
 SELECT p.project_id,first.name,1 AS depth FROM unnest($2::text[]) p(project_id) CROSS JOIN LATERAL (
  SELECT i.workflow_state_name AS name FROM issues i WHERE i.organization_id=$1 AND i.project_id=p.project_id AND i.workflow_state_name IS NOT NULL ORDER BY i.workflow_state_name LIMIT 1
 ) first
 UNION ALL
 SELECT s.project_id,next.name,s.depth+1 FROM states s CROSS JOIN LATERAL (
  SELECT i.workflow_state_name AS name FROM issues i WHERE i.organization_id=$1 AND i.project_id=s.project_id AND i.workflow_state_name>s.name ORDER BY i.workflow_state_name LIMIT 1
 ) next WHERE s.depth<=$6
), open_states AS MATERIALIZED (
 SELECT s.project_id,s.name FROM states s LEFT JOIN project_workflow_states ws ON ws.organization_id=$1 AND ws.project_id=s.project_id AND ws.name=s.name
 WHERE NOT coalesce(ws.terminal,false)
), open AS MATERIALIZED (
 (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[])
  AND i.workflow_state_name=ANY(ARRAY(SELECT name FROM open_states))
  AND (cardinality($2::text[])=1 OR (i.project_id,i.workflow_state_name) IN (SELECT project_id,name FROM open_states))
  ORDER BY i.project_id,i.workflow_state_name,i.id LIMIT $5)
 UNION ALL
 (SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=ANY($2::text[]) AND i.workflow_state_name IS NULL ORDER BY i.project_id,i.id LIMIT $5)
), active AS MATERIALIZED (
 SELECT e.project_id,e.work_item_id FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=ANY($2::text[]) AND e.recorded_at>=$3 AND e.recorded_at<$4 ORDER BY e.project_id,e.recorded_at,e.id LIMIT $5
), keyed AS (
 SELECT id FROM open
 UNION SELECT unnest($7::bigint[])
 UNION SELECT i.id FROM (SELECT DISTINCT project_id,work_item_id FROM active) a CROSS JOIN LATERAL (
  SELECT i.id FROM issues i WHERE i.organization_id=$1 AND i.project_id=a.project_id AND i.native_id=a.work_item_id LIMIT 1
 ) i
)
SELECT EXISTS(SELECT 1 FROM states WHERE depth>$6) OR (SELECT count(*) FROM open)>=$5 OR (SELECT count(*) FROM active)>=$5,
 coalesce((SELECT array_agg(id ORDER BY id) FROM keyed),'{}'::bigint[]),
 coalesce((SELECT array_agg(DISTINCT work_item_id ORDER BY work_item_id) FROM active),'{}'::text[])`, organization, projects, from, to, CandidateRows+1, maxStateNames, updated).Scan(&exceeded, &ids, &active)
	if err != nil {
		return nil, nil, pgcore.MapError(ctx, err)
	}
	if exceeded || len(ids) > CandidateRows {
		return nil, nil, reportstore.ErrPopulationExceeded
	}
	return ids, active, nil
}

func UsageCoverage(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, ids []string, current bool) (bool, error) {
	if !current {
		return importedUsageCoverage(ctx, db, organization, ids, true)
	}
	const sources = `SELECT c.source_key,c.resource_id,c.revision FROM cost_sources c WHERE c.organization_id=$1 AND c.bucket='runner_api' AND c.resource_id=ANY($2::text[])`
	rows, err := db.Query(ctx, "WITH "+FactPages("report_page", sources, "resource_id,source_key", 100000)+`,
 requested AS MATERIALIZED (SELECT DISTINCT d.id FROM unnest($2::text[]) AS d(id) LIMIT $3),
 pending AS MATERIALIZED (
  SELECT a.id,a.usage_pending_sequence FROM requested d CROSS JOIN LATERAL (SELECT a.id,a.usage_pending_sequence FROM native_attempts a WHERE a.organization_id=$1 AND a.id=d.id LIMIT 1) a
 ), projected AS MATERIALIZED (
  SELECT c.resource_id,u.known FROM report_page c LEFT JOIN LATERAL (
   SELECT u.complete AND u.revision=c.revision AND (u.usage_kind IS NOT NULL OR coalesce((SELECT false FROM attempt_usage active WHERE active.organization_id=u.organization_id AND active.attempt_id=u.attempt_id AND active.provider=u.provider AND active.model=u.model AND active.usage_sequence>0 LIMIT 1),true)) AS known
   FROM report_usage_current u WHERE u.organization_id=$1 AND u.source_key=c.source_key LIMIT 1
  ) u ON true
 )
 SELECT f.resource_id,f.known AND p.usage_pending_sequence=0 FROM projected f JOIN pending p ON p.id=f.resource_id`, organization, ids, len(ids))
	if err != nil {
		return false, pgcore.MapError(ctx, err)
	}
	seen := map[string]bool{}
	complete := true
	count := 0
	for rows.Next() {
		var id string
		var known pgtype.Bool
		if err := rows.Scan(&id, &known); err != nil {
			rows.Close()
			return false, pgcore.MapError(ctx, err)
		}
		seen[id] = true
		complete = complete && known.Valid && known.Bool
		count++
	}
	if err := rows.Err(); err != nil {
		return false, pgcore.MapError(ctx, err)
	}
	if count > 100000 {
		return false, reportstore.ErrPopulationExceeded
	}
	missing := []string{}
	for _, id := range ids {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return complete, nil
	}
	return importedUsageCoverage(ctx, db, organization, missing, complete)
}

func importedUsageCoverage(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, missing []string, complete bool) (bool, error) {
	keys, err := (usagePage{}).read(ctx, db, organization, missing, 100000)
	if err != nil {
		return false, err
	}
	type counts struct{ actual, historical int }
	observed := make(map[string]counts, len(missing))
	for i, id := range keys.attempts {
		count := observed[id]
		count.actual++
		if keys.origins[i] == "import" {
			count.historical++
		}
		observed[id] = count
	}
	rows, err := db.Query(ctx, `WITH requested AS MATERIALIZED (SELECT d.attempt_id FROM unnest($2::text[]) AS d(attempt_id) LIMIT $3)
 SELECT f.attempt_id,coalesce(c.row_count,0),a.usage_pending_sequence FROM requested f CROSS JOIN LATERAL (SELECT a.usage_pending_sequence FROM native_attempts a WHERE a.organization_id=$1 AND a.id=f.attempt_id LIMIT 1) a LEFT JOIN LATERAL (SELECT c.row_count FROM report_usage_import_coverage c WHERE c.organization_id=$1 AND c.attempt_id=f.attempt_id LIMIT 1) c ON true`, organization, missing, len(missing))
	if err != nil {
		return false, pgcore.MapError(ctx, err)
	}
	imported := map[string]bool{}
	for rows.Next() {
		var id string
		var expected, pending int
		if err := rows.Scan(&id, &expected, &pending); err != nil {
			rows.Close()
			return false, pgcore.MapError(ctx, err)
		}
		count := observed[id]
		imported[id] = count.actual > 0 && count.historical == expected && pending == 0
	}
	if err := rows.Err(); err != nil {
		return false, pgcore.MapError(ctx, err)
	}
	for _, id := range missing {
		complete = complete && imported[id]
	}
	return complete, nil
}

func CompletionCosts(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, query reportstore.CompletionCostQuery, current bool) ([]reportstore.CompletionCostFact, error) {
	projects, items := make([]string, len(query.Items)), make([]string, len(query.Items))
	seen := make(map[reportstore.CompletionCostItem]bool, len(query.Items))
	for i, item := range query.Items {
		if item.ProjectID == "" || item.WorkItemID == "" || seen[item] {
			return nil, corestore.ErrInvalidPage
		}
		seen[item] = true
		projects[i], items[i] = string(item.ProjectID), string(item.WorkItemID)
	}
	queries := pgquery.New(db)
	selected, err := queries.ReadCompletionImportAttempts(ctx, pgquery.ReadCompletionImportAttemptsParams{
		OrganizationID: organization, ProjectIds: projects, WorkItemIds: items, ItemCount: int32(len(query.Items)), Principal: query.Principal, HasSources: current,
	})
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	if len(selected) > CandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	ids := make([]string, 0, len(selected))
	for _, attempt := range selected {
		if attempt.Imported {
			ids = append(ids, attempt.ID)
		}
	}
	keys, err := (usagePage{}).read(ctx, db, organization, ids, 100000)
	if err != nil {
		return nil, err
	}
	var rows []pgquery.ReadCompletionCostsRow
	if current {
		rows, err = queries.ReadCompletionCosts(ctx, pgquery.ReadCompletionCostsParams{
			OrganizationID: organization, ProjectIds: projects, WorkItemIds: items, ItemCount: int32(len(query.Items)), Principal: query.Principal,
			CurrentTo: query.CurrentTo.UTC(), PriorTo: query.PriorTo.UTC(),
			UsageAttempts: keys.attempts, UsagePeriods: keys.periods, UsageModels: keys.models, UsageProviders: keys.providers, UsageCurrencies: keys.currencies, UsageOrigins: keys.origins,
		})
	} else {
		var imported []pgquery.ReadCompletionImportCostsRow
		imported, err = queries.ReadCompletionImportCosts(ctx, pgquery.ReadCompletionImportCostsParams{
			OrganizationID: organization, ProjectIds: projects, WorkItemIds: items, ItemCount: int32(len(query.Items)), Principal: query.Principal,
			CurrentTo: query.CurrentTo.UTC(), PriorTo: query.PriorTo.UTC(),
			UsageAttempts: keys.attempts, UsagePeriods: keys.periods, UsageModels: keys.models, UsageProviders: keys.providers, UsageCurrencies: keys.currencies, UsageOrigins: keys.origins,
		})
		for _, row := range imported {
			rows = append(rows, pgquery.ReadCompletionCostsRow(row))
		}
	}
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	out := make([]reportstore.CompletionCostFact, 0, len(rows))
	for _, row := range rows {
		if row.Exceeded {
			return nil, reportstore.ErrPopulationExceeded
		}
		if row.Cost < 0 || row.PriorCost < 0 || math.IsInf(row.Cost, 0) || math.IsNaN(row.Cost) || math.IsInf(row.PriorCost, 0) || math.IsNaN(row.PriorCost) {
			return nil, corestore.ErrInvalidPage
		}
		item := reportstore.CompletionCostItem{ProjectID: row.ProjectID, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID)}
		if !seen[item] {
			return nil, corestore.ErrTenantMismatch
		}
		out = append(out, reportstore.CompletionCostFact{CompletionCostItem: item, Cost: row.Cost, PriorCost: row.PriorCost, Complete: row.Complete})
	}
	return out, nil
}
