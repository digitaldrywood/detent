-- name: PolicyGetProjectCurrent :one
SELECT r.organization_id,r.project_id,r.policy_id,COALESCE(a.approved_by,r.approved_by) AS approved_by,
  COALESCE(a.approved_at,r.approved_at) AS approved_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,
  r.payload_content_bytes,r.gate_github_pull_request
FROM project_policies p JOIN policy_revisions r
  ON r.organization_id=p.organization_id AND r.project_id=p.project_id AND r.policy_id=p.policy_id
LEFT JOIN policy_revision_approvals a ON a.organization_id=r.organization_id AND a.project_id=r.project_id AND a.policy_id=r.policy_id
WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id);

-- name: PolicyGetProjectRevision :one
SELECT r.organization_id,r.project_id,r.policy_id,COALESCE(a.approved_by,r.approved_by) AS approved_by,
  COALESCE(a.approved_at,r.approved_at) AS approved_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,
  r.payload_content_bytes,r.gate_github_pull_request
FROM policy_revisions r LEFT JOIN policy_revision_approvals a
  ON a.organization_id=r.organization_id AND a.project_id=r.project_id AND a.policy_id=r.policy_id
WHERE r.organization_id=sqlc.arg(organization_id) AND r.project_id=sqlc.arg(project_id) AND r.policy_id=sqlc.arg(policy_id);

-- name: PolicyGetProjectRevisions :many
SELECT r.organization_id,r.project_id,r.policy_id,COALESCE(a.approved_by,r.approved_by) AS approved_by,
  COALESCE(a.approved_at,r.approved_at) AS approved_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,
  r.payload_content_bytes,r.gate_github_pull_request
FROM policy_revisions r LEFT JOIN policy_revision_approvals a
  ON a.organization_id=r.organization_id AND a.project_id=r.project_id AND a.policy_id=r.policy_id
WHERE r.organization_id=sqlc.arg(organization_id) AND r.project_id=sqlc.arg(project_id)
  AND r.policy_id=ANY(sqlc.arg(policy_ids)::text[]);

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

-- name: PolicyInsertRevisionApproval :exec
INSERT INTO policy_revision_approvals (organization_id,project_id,policy_id,approved_by,approved_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(policy_id),sqlc.arg(approved_by),sqlc.arg(approved_at))
ON CONFLICT (organization_id,project_id,policy_id) DO NOTHING;

-- name: PolicyRecordProjectObservation :execrows
INSERT INTO project_policy_observations (
  organization_id,project_id,runner_id,policy_id,observed_at,source_present,source_repository,source_commit,
  source_default_branch,source_default_branch_head,source_default_branch_reachable
)
VALUES (
  sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(runner_id),sqlc.arg(policy_id),sqlc.arg(observed_at),
  sqlc.arg(source_present),sqlc.arg(source_repository),sqlc.arg(source_commit),sqlc.arg(source_default_branch),
  sqlc.arg(source_default_branch_head),sqlc.arg(source_default_branch_reachable)
)
ON CONFLICT (organization_id,project_id,runner_id) DO UPDATE SET
  policy_id=excluded.policy_id,
  observed_at=excluded.observed_at,
  source_present=CASE WHEN NOT excluded.source_present AND project_policy_observations.policy_id=excluded.policy_id THEN project_policy_observations.source_present ELSE excluded.source_present END,
  source_repository=CASE WHEN NOT excluded.source_present AND project_policy_observations.policy_id=excluded.policy_id THEN project_policy_observations.source_repository ELSE excluded.source_repository END,
  source_commit=CASE WHEN NOT excluded.source_present AND project_policy_observations.policy_id=excluded.policy_id THEN project_policy_observations.source_commit ELSE excluded.source_commit END,
  source_default_branch=CASE WHEN NOT excluded.source_present AND project_policy_observations.policy_id=excluded.policy_id THEN project_policy_observations.source_default_branch ELSE excluded.source_default_branch END,
  source_default_branch_head=CASE WHEN NOT excluded.source_present AND project_policy_observations.policy_id=excluded.policy_id THEN project_policy_observations.source_default_branch_head ELSE excluded.source_default_branch_head END,
  source_default_branch_reachable=CASE WHEN NOT excluded.source_present AND project_policy_observations.policy_id=excluded.policy_id THEN project_policy_observations.source_default_branch_reachable ELSE excluded.source_default_branch_reachable END
WHERE project_policy_observations.observed_at<excluded.observed_at OR (
  project_policy_observations.observed_at=excluded.observed_at
  AND project_policy_observations.policy_id=excluded.policy_id
  AND project_policy_observations.source_present=excluded.source_present
  AND project_policy_observations.source_repository=excluded.source_repository
  AND project_policy_observations.source_commit=excluded.source_commit
  AND project_policy_observations.source_default_branch=excluded.source_default_branch
  AND project_policy_observations.source_default_branch_head=excluded.source_default_branch_head
  AND project_policy_observations.source_default_branch_reachable=excluded.source_default_branch_reachable
);

-- name: PolicyProjectPolicyObservations :many
WITH current_reports AS (
  SELECT o.*, r.approved_by AS revision_approved_by, r.payload_object_key, r.payload_sha256, r.payload_bytes,
    r.payload_content_bytes, r.gate_github_pull_request,
    EXISTS (SELECT 1 FROM policy_revision_approvals a WHERE a.organization_id=o.organization_id AND a.project_id=o.project_id AND a.policy_id=o.policy_id) AS previously_approved
  FROM project_policy_observations o
  JOIN policy_revisions r ON r.organization_id=o.organization_id AND r.project_id=o.project_id AND r.policy_id=o.policy_id
  JOIN runner_identities i ON i.organization_id=o.organization_id AND i.runner_id=o.runner_id
  JOIN api_tokens t ON t.organization_id=i.organization_id AND t.id=i.token_id
  JOIN projects p ON p.organization_id=o.organization_id AND p.id=o.project_id AND p.deleted_at IS NULL
  WHERE o.organization_id=sqlc.arg(organization_id) AND o.project_id=sqlc.arg(project_id)
    AND i.removed_at IS NULL AND i.expires_at>sqlc.arg(now)
    AND t.scope='worker' AND t.key_kind='service' AND t.created_at<=sqlc.arg(now)
    AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now))
    AND (i.scope='organization' OR EXISTS (
      SELECT 1 FROM token_grants g
      WHERE g.organization_id=t.organization_id AND g.token_id=t.id AND g.project_id=o.project_id
    ))
), eligible AS (
  SELECT * FROM current_reports
  WHERE (sqlc.narg(policy_id)::text IS NULL OR policy_id=sqlc.narg(policy_id)::text)
    AND (sqlc.narg(approved_policy_id)::text IS NULL OR policy_id<>sqlc.narg(approved_policy_id)::text)
), totals AS (
  SELECT count(DISTINCT policy_id)::bigint AS distinct_policy_count,
    (count(DISTINCT policy_id) FILTER (WHERE sqlc.narg(approved_policy_id)::text IS NULL OR policy_id<>sqlc.narg(approved_policy_id)::text))::bigint AS policy_count
  FROM current_reports
), grouped AS (
  SELECT eligible.*, count(*) OVER (PARTITION BY policy_id)::bigint AS runner_count FROM eligible
)
SELECT grouped.organization_id, grouped.project_id, grouped.runner_id, grouped.policy_id, grouped.observed_at,
  grouped.source_present, grouped.source_repository, grouped.source_commit, grouped.source_default_branch,
  grouped.source_default_branch_head, grouped.source_default_branch_reachable,
  grouped.revision_approved_by, grouped.payload_object_key, grouped.payload_sha256, grouped.payload_bytes,
  grouped.payload_content_bytes, grouped.gate_github_pull_request, grouped.previously_approved,
  grouped.runner_count, totals.distinct_policy_count, totals.policy_count,
  EXISTS (SELECT 1 FROM policy_revision_approvals a WHERE a.organization_id=grouped.organization_id AND a.project_id=grouped.project_id AND a.policy_id=sqlc.narg(approved_policy_id)::text AND a.approved_by='repository-default-branch') AS repository_approved
FROM grouped CROSS JOIN totals
ORDER BY grouped.observed_at DESC, grouped.runner_id
LIMIT sqlc.arg(page_size);

-- name: PolicyCurrentProjectObservation :one
SELECT EXISTS (
  SELECT 1 FROM project_policy_observations o
  JOIN runner_identities i ON i.organization_id=o.organization_id AND i.runner_id=o.runner_id
  JOIN api_tokens t ON t.organization_id=i.organization_id AND t.id=i.token_id
  JOIN projects p ON p.organization_id=o.organization_id AND p.id=o.project_id AND p.deleted_at IS NULL
  WHERE o.organization_id=sqlc.arg(organization_id) AND o.project_id=sqlc.arg(project_id) AND o.policy_id=sqlc.arg(policy_id)
    AND i.removed_at IS NULL AND i.expires_at>sqlc.arg(now)
    AND t.scope='worker' AND t.key_kind='service' AND t.created_at<=sqlc.arg(now)
    AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now))
    AND (i.scope='organization' OR EXISTS (
      SELECT 1 FROM token_grants g
      WHERE g.organization_id=t.organization_id AND g.token_id=t.id AND g.project_id=o.project_id
    ))
);

-- name: PolicyProjectWorkflowApplies :many
SELECT organization_id,project_id,id,policy_id,previous_policy_id,repository,source_commit,
  previous_definition_digest,definition_digest,runner_id,applied_by,applied_at,change_id,pull_request_url
FROM project_workflow_applies
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
  AND (sqlc.arg(before_id)::bigint=0 OR id<sqlc.arg(before_id))
ORDER BY id DESC
LIMIT sqlc.arg(page_size);

-- name: PolicyInsertWorkflowApply :one
INSERT INTO project_workflow_applies (
  organization_id,project_id,policy_id,previous_policy_id,repository,source_commit,
  previous_definition_digest,definition_digest,runner_id,applied_by,applied_at,change_id,pull_request_url
)
VALUES (
  sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(policy_id),sqlc.arg(previous_policy_id),
  sqlc.arg(repository),sqlc.arg(source_commit),sqlc.arg(previous_definition_digest),sqlc.arg(definition_digest),
  sqlc.arg(runner_id),sqlc.arg(applied_by),sqlc.arg(applied_at),sqlc.arg(change_id),sqlc.arg(pull_request_url)
)
RETURNING *;

-- name: PolicyGetReviewCurrent :one
SELECT r.organization_id,r.project_id,r.review_policy_id,r.policy_id,c.revision,
       r.updated_by,r.updated_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,r.payload_content_bytes
FROM change_review_policies c JOIN change_review_policy_revisions r
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
