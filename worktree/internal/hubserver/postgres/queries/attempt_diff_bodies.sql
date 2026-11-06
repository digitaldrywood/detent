-- name: AttemptDiffBodyReferenceCheck :one
SELECT EXISTS (
    SELECT 1 FROM attempt_diff_body_references r
    JOIN attempt_diffs d ON d.organization_id=r.organization_id AND d.id=r.diff_id
    WHERE r.organization_id=sqlc.arg(organization_id) AND r.object_key=sqlc.arg(object_key)
    AND d.project_id=sqlc.arg(project_id) AND NOT d.manifest_expired
) AS referenced,
NOT EXISTS (
    SELECT 1 FROM attempt_diffs
    WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
    AND NOT body_references_indexed AND NOT manifest_expired
) AS complete;

-- name: AttemptDiffBodyReferencesInsert :exec
INSERT INTO attempt_diff_body_references (organization_id,diff_id,object_key)
SELECT sqlc.arg(organization_id),sqlc.arg(diff_id),unnest(sqlc.arg(object_keys)::text[])
ON CONFLICT DO NOTHING;

-- name: AttemptDiffBodyReferencesComplete :execrows
UPDATE attempt_diffs SET body_references_indexed=true
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(diff_id);
