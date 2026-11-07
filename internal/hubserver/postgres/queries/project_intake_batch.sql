-- name: ProjectIntakeBatch :one
SELECT * FROM onboarding_issue_intake WHERE organization_id=$1 AND project_id=$2;

-- name: SaveProjectIntakeBatch :execrows
INSERT INTO onboarding_issue_intake(organization_id,project_id,id,revision,batch_status,hub_owned,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(id),sqlc.arg(revision),sqlc.arg(batch_status),sqlc.arg(hub_owned),sqlc.arg(payload_object_key),sqlc.arg(payload_sha256),sqlc.arg(payload_bytes),sqlc.arg(payload_content_bytes)
WHERE sqlc.arg(expected_revision)::bigint=0 OR EXISTS(SELECT 1 FROM onboarding_issue_intake WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id))
ON CONFLICT(organization_id,project_id) DO UPDATE SET id=EXCLUDED.id,revision=EXCLUDED.revision,batch_status=EXCLUDED.batch_status,hub_owned=EXCLUDED.hub_owned,payload_object_key=EXCLUDED.payload_object_key,payload_sha256=EXCLUDED.payload_sha256,payload_bytes=EXCLUDED.payload_bytes,payload_content_bytes=EXCLUDED.payload_content_bytes,next_attempt_at=clock_timestamp(),attempts=0
WHERE onboarding_issue_intake.revision=sqlc.arg(expected_revision);
