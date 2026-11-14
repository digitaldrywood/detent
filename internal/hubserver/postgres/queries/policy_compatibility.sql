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
