-- name: ArtifactReadGrantGet :one
SELECT * FROM artifact_grants WHERE organization_id=$1 AND project_id=$2 AND token_hash=$3;

-- name: ArtifactReadGrantInsert :exec
INSERT INTO artifact_grants(organization_id, project_id, token_hash, principal_id, service_id, artifact_id, revision, expires_at, hosted_session_hash)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: ArtifactReadGrantDeleteExpired :exec
DELETE FROM artifact_grants WHERE organization_id=$1 AND principal_id=$2 AND expires_at<=$3;

-- name: ArtifactReadGrantLiveCount :one
SELECT count(*) FROM (SELECT 1 FROM artifact_grants WHERE organization_id=$1 AND principal_id=$2 AND expires_at>$3 LIMIT $4) live;
