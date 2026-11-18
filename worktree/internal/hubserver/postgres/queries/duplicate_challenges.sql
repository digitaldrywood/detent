-- name: DuplicateChallenge :one
SELECT * FROM native_duplicate_challenges
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND token=sqlc.arg(token);

-- name: CreateDuplicateChallenge :exec
INSERT INTO native_duplicate_challenges(organization_id,project_id,token,principal_id,request_id,content_hash,policy_hash,source_sequence,candidate_ids,candidate_numbers,candidate_revisions,created_at,expires_at)
VALUES(sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(token),sqlc.arg(principal_id),sqlc.arg(request_id),sqlc.arg(content_hash),sqlc.arg(policy_hash),sqlc.arg(source_sequence),sqlc.arg(candidate_ids),sqlc.arg(candidate_numbers),sqlc.arg(candidate_revisions),sqlc.arg(created_at),sqlc.arg(expires_at));

-- name: ConsumeDuplicateChallenge :one
DELETE FROM native_duplicate_challenges
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND token=sqlc.arg(token)
    AND principal_id=sqlc.arg(principal_id) AND request_id=sqlc.arg(request_id)
    AND content_hash=sqlc.arg(content_hash) AND policy_hash=sqlc.arg(policy_hash)
    AND source_sequence=sqlc.arg(source_sequence) AND expires_at>sqlc.arg(now)::timestamptz
RETURNING *;

-- name: PruneDuplicateChallenges :one
SELECT prune_native_duplicate_challenges(sqlc.arg(organization_id),sqlc.arg(now)::timestamptz,sqlc.arg(batch_limit)::integer)::bigint;
