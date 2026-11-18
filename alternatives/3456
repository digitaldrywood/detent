-- name: ClaimVersionPublications :many
SELECT q.request_index::bigint AS request_index,sqlc.embed(p)
FROM unnest(sqlc.arg(version_ids)::text[]) WITH ORDINALITY q(version_id,request_index)
CROSS JOIN LATERAL (SELECT * FROM native_pr_publications p WHERE p.organization_id=sqlc.arg(organization_id) AND p.version_id=q.version_id OFFSET 0) p
CROSS JOIN LATERAL (SELECT id FROM change_requests c WHERE c.organization_id=p.organization_id AND c.id=p.change_id AND c.project_id=sqlc.arg(project_id) OFFSET 0) c
ORDER BY q.request_index;

-- name: ClaimVersionLandingObservations :many
SELECT q.request_index::bigint AS request_index,sqlc.embed(e),
COALESCE(w.from_state,e.from_state)::text AS landing_from_state,COALESCE(w.to_state,'')::text AS landing_target_state
FROM unnest(sqlc.arg(work_item_ids)::text[]) WITH ORDINALITY q(work_item_id,request_index)
CROSS JOIN LATERAL (
 SELECT * FROM collaboration_events e
 WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id) AND e.work_item_id=q.work_item_id
 AND (e.landing_change_id,e.landing_version_id,e.landing_head_sha,e.sequence)>=((sqlc.arg(change_ids)::text[])[q.request_index],(sqlc.arg(version_ids)::text[])[q.request_index],(sqlc.arg(head_shas)::text[])[q.request_index],0::bigint)
 AND (e.landing_change_id,e.landing_version_id,e.landing_head_sha,e.sequence)<=((sqlc.arg(change_ids)::text[])[q.request_index],(sqlc.arg(version_ids)::text[])[q.request_index],(sqlc.arg(head_shas)::text[])[q.request_index],9223372036854775807::bigint)
 AND e.landing_version_id<>'' ORDER BY e.landing_change_id,e.landing_version_id,e.landing_head_sha,e.sequence DESC LIMIT 1
) e
LEFT JOIN LATERAL (
 SELECT from_state,to_state FROM collaboration_events transition
 WHERE transition.organization_id=e.organization_id AND transition.project_id=e.project_id AND transition.work_item_id=e.work_item_id
 AND transition.type='workflow.transitioned'
 AND (transition.change_id,transition.version_id,transition.head_sha,transition.sequence)>=(e.landing_change_id,e.landing_version_id,e.landing_head_sha,0::bigint)
 AND (transition.change_id,transition.version_id,transition.head_sha,transition.sequence)<=(e.landing_change_id,e.landing_version_id,e.landing_head_sha,9223372036854775807::bigint)
 ORDER BY transition.change_id,transition.version_id,transition.head_sha,transition.sequence DESC LIMIT 1
) w ON true
ORDER BY q.request_index;
