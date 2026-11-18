-- name: ReleaseAudit :one
SELECT * FROM release_audit_checkpoints WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id);

-- name: ReleaseAudits :many
SELECT c.* FROM release_audit_heads h JOIN release_audit_checkpoints c
ON c.organization_id=h.organization_id AND c.project_id=h.project_id AND c.id=h.checkpoint_id
WHERE h.organization_id=sqlc.arg(organization_id) AND h.project_id=sqlc.arg(project_id) AND h.checkpoint_id>sqlc.arg(after_id)
ORDER BY h.checkpoint_id LIMIT 21;

-- name: AdvanceReleaseAuditHead :exec
INSERT INTO release_audit_heads(organization_id,project_id,run_id,run_attempt,job_id,checkpoint_id,sequence)
VALUES(sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(run_id),sqlc.arg(run_attempt),sqlc.arg(job_id),sqlc.arg(checkpoint_id),sqlc.arg(sequence))
ON CONFLICT(organization_id,project_id,run_id,run_attempt,job_id) DO UPDATE SET checkpoint_id=EXCLUDED.checkpoint_id,sequence=EXCLUDED.sequence WHERE release_audit_heads.sequence<EXCLUDED.sequence;

-- name: ReleaseAuditParts :many
SELECT * FROM release_audit_parts WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND checkpoint_id=sqlc.arg(checkpoint_id) ORDER BY part_index LIMIT 162;

-- name: CreateReleaseAudit :execrows
INSERT INTO release_audit_checkpoints (organization_id,project_id,id,repository,commit_sha,release_series,run_id,run_attempt,job_id,run_url,sequence,status,bundle_commit,bundle_digest,allowance_invocations,allowance_input,allowance_output,allowance_cached,invocations,input_tokens,output_tokens,cached_input_tokens,digest,bytes,recorded_at) VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(id),sqlc.arg(repository),sqlc.arg(commit_sha),sqlc.arg(release_series),sqlc.arg(run_id),sqlc.arg(run_attempt),sqlc.arg(job_id),sqlc.arg(run_url),sqlc.arg(sequence),sqlc.arg(status),sqlc.arg(bundle_commit),sqlc.arg(bundle_digest),sqlc.arg(allowance_invocations),sqlc.arg(allowance_input),sqlc.arg(allowance_output),sqlc.arg(allowance_cached),sqlc.arg(invocations),sqlc.arg(input_tokens),sqlc.arg(output_tokens),sqlc.arg(cached_input_tokens),sqlc.arg(digest),sqlc.arg(bytes),sqlc.arg(recorded_at)) ON CONFLICT DO NOTHING;

-- name: CreateReleaseAuditPart :exec
INSERT INTO release_audit_parts (organization_id,project_id,checkpoint_id,part_index,attachment_id,byte_offset,bytes,digest) VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(checkpoint_id),sqlc.arg(part_index),sqlc.arg(attachment_id),sqlc.arg(byte_offset),sqlc.arg(bytes),sqlc.arg(digest));

-- name: RetainReleaseAuditAttachment :exec
UPDATE attachments SET retention_class='organization'
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id);

-- name: LockReleaseAuditAttachment :one
SELECT size,sha256,content_type,deleted_at,object_deleted_at FROM attachments
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND id=sqlc.arg(id) FOR UPDATE;
