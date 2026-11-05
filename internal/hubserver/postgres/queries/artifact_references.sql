-- name: ArtifactReferenceGet :one
SELECT * FROM artifact_references
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id)
AND artifact_id=sqlc.arg(artifact_id) AND revision=sqlc.arg(revision);

-- name: ArtifactReferenceInsert :one
INSERT INTO artifact_references (organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,schema_version,run_id,attempt_id,version_id,sha256,kind,state,availability,bytes,objects,expires_at,observed_at)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(work_item_id),sqlc.arg(service_id),sqlc.arg(artifact_id),sqlc.arg(revision),sqlc.arg(manifest_id),sqlc.arg(schema_version),sqlc.arg(run_id),sqlc.arg(attempt_id),sqlc.arg(version_id),sqlc.arg(sha256),sqlc.arg(kind),sqlc.arg(state),sqlc.arg(availability),sqlc.arg(bytes),sqlc.arg(objects),sqlc.arg(expires_at),sqlc.arg(observed_at))
ON CONFLICT (organization_id,project_id,artifact_id,revision) DO NOTHING RETURNING *;

-- name: ArtifactReferenceListLatest :many
SELECT organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,schema_version,run_id,attempt_id,version_id,sha256,kind,state,availability,bytes,objects,expires_at,observed_at
FROM (
    SELECT r.*,row_number() OVER (PARTITION BY r.artifact_id ORDER BY r.revision DESC) AS artifact_revision_rank
    FROM artifact_references r
    WHERE r.organization_id=sqlc.arg(organization_id) AND r.project_id=sqlc.arg(project_id) AND r.work_item_id=sqlc.arg(work_item_id)
) latest
WHERE artifact_revision_rank=1
ORDER BY artifact_id
LIMIT sqlc.arg(page_limit);

-- name: ArtifactVersionAttemptMatches :one
SELECT EXISTS (
    SELECT 1 FROM change_versions v
    JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id
    WHERE v.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id)
    AND v.id=sqlc.arg(version_id) AND v.attempt_id=sqlc.arg(attempt_id)
);
