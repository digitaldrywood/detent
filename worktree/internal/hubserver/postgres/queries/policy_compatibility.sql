-- name: PolicyInsertRevisionCompatibility :execrows
INSERT INTO policy_revision_compatibility (organization_id,project_id,policy_id,payload_sha256,compatibility_schema,gate_github_pull_request,
  gate_landing_mode,gate_kind,gate_landing_command_digest,gate_github_ci_digest,gate_plan_review,gate_automated_review,
  gate_required_checks,gate_validator,gate_security_audit,gate_human_review,gate_plan_enabled)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(policy_id),sqlc.arg(payload_sha256),1,sqlc.arg(gate_github_pull_request),
  sqlc.arg(gate_landing_mode),sqlc.arg(gate_kind),sqlc.arg(gate_landing_command_digest),sqlc.arg(gate_github_ci_digest),sqlc.arg(gate_plan_review),sqlc.arg(gate_automated_review),
  sqlc.arg(gate_required_checks),sqlc.arg(gate_validator),sqlc.arg(gate_security_audit),sqlc.arg(gate_human_review),sqlc.arg(gate_plan_enabled))
ON CONFLICT (organization_id,project_id,policy_id) DO NOTHING;

-- name: PolicyGetRevisionCompatibility :many
SELECT c.policy_id,c.payload_sha256,c.gate_github_pull_request,c.gate_landing_mode,c.gate_kind,c.gate_landing_command_digest,c.gate_github_ci_digest,
  c.gate_plan_review,c.gate_automated_review,c.gate_required_checks,c.gate_validator,c.gate_security_audit,c.gate_human_review,c.gate_plan_enabled
FROM policy_revision_compatibility c
WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id) AND c.policy_id=ANY(sqlc.arg(policy_ids)::text[])
ORDER BY c.policy_id;

-- name: PolicyInsertReviewCompatibility :execrows
INSERT INTO review_policy_revision_compatibility (organization_id,project_id,review_policy_id,policy_id,payload_sha256,compatibility_schema,require_review,required_check_count)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(review_policy_id),sqlc.arg(policy_id),sqlc.arg(payload_sha256),1,sqlc.arg(require_review),sqlc.arg(required_check_count))
ON CONFLICT (organization_id,project_id,review_policy_id) DO NOTHING;

-- name: PolicyInsertReviewChecks :execrows
INSERT INTO review_policy_revision_checks (organization_id,project_id,review_policy_id,ordinal,name,principal_id,workflow_id,workflow_sha256,source,max_age_seconds)
SELECT sqlc.arg(organization_id)::text,sqlc.arg(project_id)::text,sqlc.arg(review_policy_id)::text,
  unnest(sqlc.arg(ordinals)::integer[]),unnest(sqlc.arg(names)::text[]),unnest(sqlc.arg(principal_ids)::text[]),unnest(sqlc.arg(workflow_ids)::text[]),
  unnest(sqlc.arg(workflow_sha256s)::text[]),unnest(sqlc.arg(sources)::text[]),unnest(sqlc.arg(max_ages)::bigint[]);

-- name: PolicyGetReviewCompatibility :many
SELECT review_policy_id,policy_id,payload_sha256,require_review,required_check_count
FROM review_policy_revision_compatibility
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND review_policy_id=ANY(sqlc.arg(review_policy_ids)::text[])
ORDER BY review_policy_id;

-- name: PolicyGetReviewChecks :many
SELECT review_policy_id,ordinal,name,principal_id,workflow_id,workflow_sha256,source,max_age_seconds
FROM review_policy_revision_checks
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND review_policy_id=ANY(sqlc.arg(review_policy_ids)::text[])
ORDER BY review_policy_id,ordinal;

-- name: PolicyGetReviewCurrentFacts :one
SELECT r.organization_id,r.project_id,r.review_policy_id,r.policy_id,c.revision,
       r.updated_by,r.updated_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,r.payload_content_bytes,
       (f.review_policy_id IS NOT NULL)::boolean AS has_facts,coalesce(f.require_review,false)::boolean AS require_review,
       coalesce(f.required_check_count,0)::integer AS required_check_count,k.ordinals,k.names,k.principal_ids,k.workflow_ids,k.workflow_sha256s,k.sources,k.max_ages
FROM change_review_policies c JOIN change_review_policy_revisions r
  ON r.organization_id=c.organization_id AND r.project_id=c.project_id AND r.review_policy_id=c.review_policy_id
LEFT JOIN review_policy_revision_compatibility f
  ON f.organization_id=r.organization_id AND f.project_id=r.project_id AND f.review_policy_id=r.review_policy_id AND f.policy_id=r.policy_id AND f.payload_sha256=r.payload_sha256
CROSS JOIN LATERAL (
  SELECT coalesce(array_agg(x.ordinal ORDER BY x.ordinal),'{}')::integer[] AS ordinals,coalesce(array_agg(x.name ORDER BY x.ordinal),'{}')::text[] AS names,
         coalesce(array_agg(x.principal_id ORDER BY x.ordinal),'{}')::text[] AS principal_ids,coalesce(array_agg(x.workflow_id ORDER BY x.ordinal),'{}')::text[] AS workflow_ids,
         coalesce(array_agg(x.workflow_sha256 ORDER BY x.ordinal),'{}')::text[] AS workflow_sha256s,coalesce(array_agg(x.source ORDER BY x.ordinal),'{}')::text[] AS sources,
         coalesce(array_agg(x.max_age_seconds ORDER BY x.ordinal),'{}')::bigint[] AS max_ages
  FROM review_policy_revision_checks x
  WHERE x.organization_id=f.organization_id AND x.project_id=f.project_id AND x.review_policy_id=f.review_policy_id
) k
WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id);
