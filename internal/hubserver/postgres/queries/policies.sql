-- name: PolicyGetProjectCurrent :one
SELECT r.* FROM project_policies p JOIN policy_revisions r
  ON r.organization_id=p.organization_id AND r.project_id=p.project_id AND r.policy_id=p.policy_id
WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id);

-- name: PolicyGetProjectRevision :one
SELECT * FROM policy_revisions WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND policy_id=sqlc.arg(policy_id);

-- name: PolicyInsertProjectRevision :one
INSERT INTO policy_revisions (organization_id,project_id,policy_id,gate_github_pull_request,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(policy_id),sqlc.narg(gate_github_pull_request),sqlc.arg(approved_by),sqlc.arg(approved_at),sqlc.arg(payload_object_key),sqlc.arg(payload_sha256),sqlc.arg(payload_bytes),sqlc.arg(payload_content_bytes))
ON CONFLICT (organization_id,project_id,policy_id) DO NOTHING RETURNING *;

-- name: PolicySetProjectCurrent :execrows
INSERT INTO project_policies (organization_id,project_id,policy_id)
SELECT sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(policy_id)
WHERE (sqlc.narg(expected_policy_id)::text IS NULL AND NOT EXISTS (
  SELECT 1 FROM project_policies WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
)) OR EXISTS (
  SELECT 1 FROM project_policies WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND policy_id=sqlc.narg(expected_policy_id)::text
)
ON CONFLICT (organization_id,project_id) DO UPDATE SET policy_id=excluded.policy_id
WHERE project_policies.policy_id=sqlc.narg(expected_policy_id)::text;

-- name: PolicyGetReviewCurrent :one
SELECT r.* FROM change_review_policies c JOIN change_review_policy_revisions r
  ON r.organization_id=c.organization_id AND r.project_id=c.project_id AND r.review_policy_id=c.review_policy_id
WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id);

-- name: PolicyGetReviewRevision :one
SELECT * FROM change_review_policy_revisions WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND review_policy_id=sqlc.arg(review_policy_id);

-- name: PolicyInsertReviewRevision :one
INSERT INTO change_review_policy_revisions (organization_id,project_id,review_policy_id,policy_id,revision,updated_by,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(review_policy_id),sqlc.arg(policy_id),sqlc.arg(revision),sqlc.arg(updated_by),sqlc.arg(updated_at),sqlc.arg(payload_object_key),sqlc.arg(payload_sha256),sqlc.arg(payload_bytes),sqlc.arg(payload_content_bytes))
ON CONFLICT (organization_id,project_id,review_policy_id) DO NOTHING RETURNING *;

-- name: PolicySetReviewCurrent :execrows
INSERT INTO change_review_policies (organization_id,project_id,review_policy_id,policy_id,revision)
SELECT sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(review_policy_id),sqlc.arg(policy_id),sqlc.arg(revision)
WHERE (sqlc.narg(expected_review_policy_id)::text IS NULL AND NOT EXISTS (
  SELECT 1 FROM change_review_policies WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
)) OR EXISTS (
  SELECT 1 FROM change_review_policies WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND review_policy_id=sqlc.narg(expected_review_policy_id)::text
)
ON CONFLICT (organization_id,project_id) DO UPDATE SET review_policy_id=excluded.review_policy_id,policy_id=excluded.policy_id,revision=excluded.revision
WHERE change_review_policies.review_policy_id=sqlc.narg(expected_review_policy_id)::text;
