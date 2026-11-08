-- name: ConfigurationHeader :one
SELECT * FROM runner_configuration_snapshots WHERE organization_id=$1 AND runner_id=$2;

-- name: LockConfigurationRunner :one
SELECT r.revision,r.token_id,r.enrollment_id FROM runner_identities r JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
WHERE r.organization_id=$1 AND r.runner_id=$2 AND r.removed_at IS NULL AND r.expires_at>$3 AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL AND t.created_at<=$3 AND (t.expires_at IS NULL OR t.expires_at>$3) FOR UPDATE OF r;

-- name: CreateConfigurationHeader :one
INSERT INTO runner_configuration_snapshots(organization_id,runner_id,source_revision,defaults_object_key,defaults_sha256,defaults_bytes,defaults_content_bytes,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8) ON CONFLICT DO NOTHING RETURNING *;

-- name: AdvanceConfigurationHeader :one
UPDATE runner_configuration_snapshots SET revision=revision+1,source_revision=sqlc.arg(source_revision),defaults_object_key=sqlc.narg(defaults_object_key),defaults_sha256=sqlc.arg(defaults_sha256),defaults_bytes=sqlc.arg(defaults_bytes),defaults_content_bytes=sqlc.arg(defaults_content_bytes),updated_at=sqlc.arg(updated_at)
WHERE organization_id=sqlc.arg(organization_id) AND runner_id=sqlc.arg(runner_id) AND revision=sqlc.arg(expected_revision) AND source_revision=sqlc.arg(expected_source_revision) RETURNING *;

-- name: AuthorizedConfigurationProjects :many
SELECT p.id FROM runner_identities r JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id JOIN projects p ON p.organization_id=r.organization_id AND p.id=ANY(sqlc.arg(project_ids)::text[])
WHERE r.organization_id=sqlc.arg(organization_id) AND r.runner_id=sqlc.arg(runner_id) AND r.removed_at IS NULL AND r.expires_at>sqlc.arg(now)::timestamptz AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL AND t.created_at<=sqlc.arg(now) AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now)) AND p.deleted_at IS NULL AND (r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id)) ORDER BY p.id;

-- name: ConfigurationProjects :many
SELECT c.* FROM runner_project_configurations c JOIN runner_identities r ON r.organization_id=c.organization_id AND r.runner_id=c.runner_id JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id JOIN projects p ON p.organization_id=c.organization_id AND p.id=c.project_id
WHERE c.organization_id=sqlc.arg(organization_id) AND c.runner_id=sqlc.arg(runner_id) AND c.project_id>sqlc.arg(after_project)::text AND r.removed_at IS NULL AND r.expires_at>sqlc.arg(now)::timestamptz AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL AND t.created_at<=sqlc.arg(now) AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now)) AND p.deleted_at IS NULL AND (r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id)) ORDER BY c.project_id LIMIT sqlc.arg(page_limit);

-- name: ApplyConfigurationProject :one
INSERT INTO runner_project_configurations(organization_id,runner_id,project_id,config_revision,policy_id,state,paused,allow_local_binding,local_intake_enabled,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,updated_at)
SELECT sqlc.arg(organization_id),sqlc.arg(runner_id),sqlc.arg(project_id),sqlc.arg(config_revision),sqlc.arg(policy_id),sqlc.arg(state),sqlc.arg(paused),sqlc.arg(allow_local_binding),sqlc.arg(local_intake_enabled),sqlc.arg(payload_object_key),sqlc.arg(payload_sha256),sqlc.arg(payload_bytes),sqlc.arg(payload_content_bytes),sqlc.arg(updated_at)
WHERE (sqlc.arg(policy_id)::text='' AND NOT EXISTS(SELECT 1 FROM project_policies existing WHERE existing.organization_id=sqlc.arg(organization_id) AND existing.project_id=sqlc.arg(project_id))) OR EXISTS(SELECT 1 FROM project_policies p WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id) AND p.policy_id=sqlc.arg(policy_id))
ON CONFLICT(organization_id,runner_id,project_id) DO UPDATE SET config_revision=EXCLUDED.config_revision,policy_id=EXCLUDED.policy_id,state=EXCLUDED.state,paused=EXCLUDED.paused,allow_local_binding=EXCLUDED.allow_local_binding,local_intake_enabled=EXCLUDED.local_intake_enabled,payload_object_key=EXCLUDED.payload_object_key,payload_sha256=EXCLUDED.payload_sha256,payload_bytes=EXCLUDED.payload_bytes,payload_content_bytes=EXCLUDED.payload_content_bytes,revision=runner_project_configurations.revision+1,updated_at=EXCLUDED.updated_at RETURNING *;

-- name: ObserveConfigurationProject :one
UPDATE runner_project_configurations SET observed_config_revision=sqlc.arg(config_revision),observed_policy_id=sqlc.arg(policy_id),observed_registered=sqlc.arg(registered),observed_paused=sqlc.arg(paused),observed_draining=sqlc.arg(draining),observed_at=sqlc.arg(observed_at)
WHERE organization_id=sqlc.arg(organization_id) AND runner_id=sqlc.arg(runner_id) AND project_id=sqlc.arg(project_id) AND config_revision=sqlc.arg(config_revision) AND policy_id=sqlc.arg(policy_id) AND (observed_at IS NULL OR observed_at<=sqlc.arg(observed_at)) RETURNING *;
