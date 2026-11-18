-- name: HostedArtifactPublisherBound :one
SELECT EXISTS (
 SELECT 1 FROM artifact_services s
 JOIN token_grants g ON g.organization_id=s.organization_id AND g.project_id=s.project_id AND g.token_id=s.publisher_token_id
 JOIN api_tokens t ON t.organization_id=g.organization_id AND t.id=g.token_id
 WHERE s.organization_id=sqlc.arg(organization_id) AND s.id=sqlc.arg(service_id) AND s.publisher_token_id=sqlc.arg(publisher_id)
 AND s.binding_mode='hosted' AND s.hosted_opt_in AND t.revoked_at IS NULL
 AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now)::timestamptz)
);

-- name: HostedArtifactUsageEnsure :exec
INSERT INTO hosted_artifact_usage (organization_id,service_id,observed_at,retained_bytes,reserved_bytes)
VALUES ($1,$2,'1970-01-01 00:00:00+00',0,0) ON CONFLICT (organization_id) DO NOTHING;

-- name: HostedArtifactUsageLock :one
SELECT service_id,observed_at FROM hosted_artifact_usage WHERE organization_id=$1 FOR UPDATE;

-- name: HostedArtifactUsageUpdate :exec
UPDATE hosted_artifact_usage SET observed_at=$2,retained_bytes=$3,reserved_bytes=$4 WHERE organization_id=$1;

-- name: HostedArtifactUsageMetric :exec
INSERT INTO hosted_usage_windows (organization_id,window_start,metric,amount) VALUES ($1,$2,$3,$4)
ON CONFLICT (organization_id,window_start,metric) DO UPDATE SET amount=GREATEST(hosted_usage_windows.amount,excluded.amount);
