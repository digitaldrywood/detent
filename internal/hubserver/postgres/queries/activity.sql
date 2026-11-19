-- name: ActivityLatestRunEvents :many
SELECT latest.*
FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
CROSS JOIN LATERAL (
 SELECT * FROM collaboration_events e
 WHERE e.organization_id=a.organization_id AND e.attempt_id=a.id AND e.attempt_id<>''
 ORDER BY e.sequence DESC LIMIT 1
) latest
WHERE a.organization_id=sqlc.arg(organization_id) AND a.id=ANY(sqlc.arg(attempt_ids)::text[]) AND latest.project_id=i.project_id;

-- name: ActivityIssueLinks :many
SELECT requested.id::bigint AS id,wi.workspace_id,coalesce(linked.change_id,'')::text AS change_id,coalesce(v.external_url,'')::text AS publication_url
FROM (SELECT r.id,(sqlc.arg(project_ids)::text[])[r.n] AS project_id,(sqlc.arg(work_item_ids)::text[])[r.n] AS native_id FROM unnest(sqlc.arg(issue_ids)::bigint[]) WITH ORDINALITY r(id,n) LIMIT sqlc.arg(issue_count)) requested
LEFT JOIN workspace_items wi ON wi.organization_id=sqlc.arg(organization_id) AND wi.project_id=requested.project_id AND wi.work_item_id=requested.native_id
LEFT JOIN LATERAL (
 SELECT change_id FROM change_issue_links l
 WHERE l.organization_id=sqlc.arg(organization_id) AND l.project_id=requested.project_id AND l.work_item_id=requested.native_id
 ORDER BY l.created_at DESC,l.change_id DESC LIMIT 1
) linked ON true
LEFT JOIN LATERAL (
 SELECT c.organization_id,c.current_version_id FROM change_requests c WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=requested.project_id AND c.id=linked.change_id LIMIT 1
) c ON true
LEFT JOIN change_versions v ON v.organization_id=c.organization_id AND v.id=c.current_version_id;

-- name: ActivityHourlyWindowPopulation :one
WITH undated AS MATERIALIZED (
 SELECT v.organization_id,v.project_id,v.change_id FROM change_versions v
 WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND NOT v.landing_history_known
   AND v.created_at<sqlc.arg(window_to)::timestamptz
 ORDER BY v.created_at DESC,v.id DESC LIMIT sqlc.arg(landing_limit)
)
SELECT (
 SELECT count(*) FROM (
  SELECT 1 FROM native_attempts a
  WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.status<>'running'
    AND a.updated_at>=sqlc.arg(window_from)::timestamptz AND a.updated_at<sqlc.arg(window_to)::timestamptz
  ORDER BY a.updated_at DESC,a.id DESC LIMIT sqlc.arg(population_limit)
 ) attempts
) AS attempts, (
 SELECT count(*) FROM (
  SELECT 1 FROM change_versions v
  WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND v.landing_history_known
    AND v.landed_at>=sqlc.arg(window_from)::timestamptz AND v.landed_at<sqlc.arg(window_to)::timestamptz
  ORDER BY v.landed_at DESC,v.change_id DESC,v.id DESC LIMIT sqlc.arg(landing_limit)
 ) landings
) AS landings, (SELECT count(*) FROM undated) AS undated, EXISTS (
 SELECT 1 FROM undated v
 JOIN change_requests c ON c.organization_id=v.organization_id AND c.project_id=v.project_id AND c.id=v.change_id
 WHERE sqlc.arg(principal)::text='' OR EXISTS (
  SELECT 1 FROM issues owned WHERE owned.organization_id=c.organization_id AND owned.project_id=c.project_id AND owned.native_id=c.work_item_id AND owned.actor_principal_id=sqlc.arg(principal)
 )
) AS undated_landing_history;

-- name: ActivityHourlyOutcomes :many
WITH attempts AS MATERIALIZED (
 SELECT a.organization_id,a.id,a.issue_id,a.project_id,a.status,a.started_at,a.updated_at,
        a.verdict_outcome,a.verdict_success,a.verdict_reason,a.verdict_next_step,a.verdict_disposition
 FROM native_attempts a
 WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.status<>'running'
   AND a.updated_at>=sqlc.arg(window_from)::timestamptz AND a.updated_at<sqlc.arg(window_to)::timestamptz
 ORDER BY a.updated_at DESC,a.id DESC LIMIT sqlc.arg(population_limit)
), facts AS (
 SELECT date_trunc('hour',a.updated_at,'UTC')::timestamptz AS hour,
        coalesce(nullif(f.activity_stage,''),'unknown')::text AS ending_stage,
        a.status AS outcome,
        coalesce(nullif(f.failure_kind,''),nullif(f.failure_class,''),CASE WHEN a.status='succeeded' THEN 'not_applicable' ELSE 'unknown' END)::text AS failure_reason,
        coalesce(nullif(a.verdict_outcome,''),'unavailable')::text AS verdict,a.verdict_success,coalesce(nullif(a.verdict_reason,''),'unknown')::text AS reason,
        coalesce(nullif(a.verdict_next_step,''),'unknown')::text AS next_step,coalesce(nullif(a.verdict_disposition,''),'unknown')::text AS disposition,
        CASE WHEN recent.activity_no_change_status='accepted' OR f.activity_no_change_status='accepted' THEN 'accepted'
             ELSE coalesce(nullif(nullif(recent.activity_no_change_status,''),'unknown'),nullif(f.activity_no_change_status,''),'unknown')
        END AS no_change_status,
        greatest(0,extract(epoch FROM a.updated_at-a.started_at))::double precision AS elapsed
 FROM attempts a
 LEFT JOIN report_attempt_facts ref ON ref.organization_id=a.organization_id AND ref.attempt_id=a.id
 LEFT JOIN report_event_facts f ON f.organization_id=ref.organization_id AND f.event_id=ref.outcome_event_id
 LEFT JOIN report_event_facts recent ON recent.organization_id=ref.organization_id AND recent.event_id=ref.latest_event_id
 WHERE sqlc.arg(principal)::text='' OR EXISTS (
  SELECT 1 FROM issues i WHERE i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.id=a.issue_id AND i.actor_principal_id=sqlc.arg(principal)
 )
)
SELECT hour,ending_stage,outcome,failure_reason,verdict,verdict_success,reason,next_step,disposition,
       count(*) AS attempts,coalesce(sum(elapsed),0)::double precision AS elapsed,
       count(*) FILTER (WHERE no_change_status='accepted') AS accepted_no_change,
       count(*) FILTER (WHERE no_change_status='unknown') AS unknown_no_change
FROM facts
GROUP BY hour,ending_stage,outcome,failure_reason,verdict,verdict_success,reason,next_step,disposition
ORDER BY hour,ending_stage,outcome,failure_reason,verdict,verdict_success,reason,next_step,disposition
LIMIT sqlc.arg(group_limit);

-- name: ActivityHourlyAttemptIDs :many
SELECT a.id,i.native_id::text AS work_item_id,a.updated_at AS finished_at
FROM (
 SELECT selected.organization_id,selected.project_id,selected.issue_id,selected.id,selected.updated_at
 FROM native_attempts selected
 WHERE selected.organization_id=sqlc.arg(organization_id) AND selected.project_id=sqlc.arg(project_id) AND selected.status<>'running'
   AND selected.updated_at>=sqlc.arg(window_from)::timestamptz AND selected.updated_at<sqlc.arg(window_to)::timestamptz
 ORDER BY selected.updated_at DESC,selected.id DESC LIMIT sqlc.arg(population_limit)
) a
JOIN work_item_identities i ON i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.id=a.issue_id
WHERE sqlc.arg(principal)::text='' OR EXISTS (
 SELECT 1 FROM issues owned WHERE owned.organization_id=a.organization_id AND owned.project_id=a.project_id AND owned.id=a.issue_id AND owned.actor_principal_id=sqlc.arg(principal)
)
ORDER BY a.updated_at DESC,a.id DESC LIMIT sqlc.arg(reference_limit);

-- name: ActivityHourlyLandings :many
WITH landing_versions AS MATERIALIZED (
 SELECT v.organization_id,v.project_id,v.id,v.change_id,v.landed_at,v.landed_merge_sha,v.landed_base_ref
 FROM change_versions v
 WHERE v.organization_id=sqlc.arg(organization_id) AND v.project_id=sqlc.arg(project_id) AND v.landing_history_known
   AND v.landed_at>=sqlc.arg(window_from)::timestamptz AND v.landed_at<sqlc.arg(window_to)::timestamptz
 ORDER BY v.landed_at DESC,v.change_id DESC,v.id DESC LIMIT sqlc.arg(landing_limit)
)
SELECT v.id,v.change_id,coalesce(proof.attempt_id,'')::text AS attempt_id,v.landed_at,
       least(cohort.observed,sqlc.arg(history_limit)::bigint-1)::bigint AS linked_attempts,
       CASE WHEN proof.attempt_id IS NULL THEN 'unknown'
            WHEN cohort.observed>=sqlc.arg(history_limit)::bigint OR cohort.terminal<cohort.observed
            OR (prior.fencing_token IS NULL AND (prior.oldest_examined IS NOT NULL OR NOT EXISTS (
             SELECT 1 FROM collaboration_events e WHERE e.organization_id=v.organization_id AND e.project_id=v.project_id
               AND e.work_item_id=i.native_id AND e.type='issue.created' AND e.recorded_at<=v.landed_at
            ))) THEN 'partial' ELSE 'complete' END::text AS history_coverage
FROM landing_versions v
JOIN change_requests c ON c.organization_id=v.organization_id AND c.project_id=v.project_id AND c.id=v.change_id
JOIN work_item_identities i ON i.organization_id=v.organization_id AND i.project_id=v.project_id AND i.native_id=c.work_item_id
LEFT JOIN LATERAL (
 SELECT r.attempt_id,r.issue_id,r.fencing_token,r.repository_identity
 FROM change_landing_receipts r
 WHERE r.organization_id=v.organization_id AND r.version_id=v.id AND r.project_id=v.project_id AND r.change_id=v.change_id
   AND r.merge_sha=v.landed_merge_sha AND r.base_ref=v.landed_base_ref
 ORDER BY r.fencing_token,r.attempt_id LIMIT 1
) proof ON true
LEFT JOIN LATERAL (
 SELECT max(r.fencing_token) FILTER (WHERE pv.id IS NOT NULL AND NOT EXISTS (
   SELECT 1 FROM change_landing_receipts first
   WHERE first.organization_id=v.organization_id AND first.version_id=r.version_id
     AND first.merge_sha=r.merge_sha AND first.base_ref=r.base_ref AND first.fencing_token<r.fencing_token
 )) AS fencing_token,CASE WHEN count(*)>=sqlc.arg(history_limit)::bigint THEN min(r.fencing_token) END AS oldest_examined
 FROM (
  SELECT candidate.version_id,candidate.change_id,candidate.fencing_token,candidate.merge_sha,candidate.base_ref
  FROM change_landing_receipts candidate
  WHERE candidate.organization_id=v.organization_id AND candidate.project_id=v.project_id AND candidate.issue_id=proof.issue_id
    AND candidate.repository_identity=proof.repository_identity AND candidate.fencing_token<proof.fencing_token
  ORDER BY candidate.fencing_token DESC,candidate.attempt_id DESC LIMIT sqlc.arg(history_limit)
 ) r
 LEFT JOIN change_versions pv ON pv.organization_id=v.organization_id AND pv.project_id=v.project_id
   AND pv.id=r.version_id AND pv.change_id=r.change_id AND pv.landing_history_known
   AND pv.landed_at<=v.landed_at AND pv.landed_merge_sha=r.merge_sha AND pv.landed_base_ref=r.base_ref
) prior ON true
CROSS JOIN LATERAL (
 SELECT count(*) AS observed,count(ref.outcome_event_id) FILTER (WHERE a.status<>'running') AS terminal
 FROM (
  SELECT a.id,a.organization_id,a.status FROM native_attempts a
  WHERE a.organization_id=v.organization_id AND a.project_id=v.project_id AND a.issue_id=proof.issue_id
    AND a.fencing_token>coalesce(prior.fencing_token,prior.oldest_examined-1,0) AND a.fencing_token<=proof.fencing_token
  ORDER BY a.fencing_token DESC LIMIT sqlc.arg(history_limit)
 ) a
 LEFT JOIN report_attempt_facts ref ON ref.organization_id=a.organization_id AND ref.attempt_id=a.id
) cohort
WHERE sqlc.arg(principal)::text='' OR EXISTS (
 SELECT 1 FROM issues owned WHERE owned.organization_id=v.organization_id AND owned.project_id=v.project_id AND owned.id=i.id AND owned.actor_principal_id=sqlc.arg(principal)
)
ORDER BY v.landed_at DESC,v.change_id DESC,v.id DESC;
