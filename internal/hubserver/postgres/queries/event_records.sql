-- name: RecordGetEvent :one
SELECT * FROM collaboration_events WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND id=sqlc.arg(id);

-- name: RecordInsertEvent :one
INSERT INTO collaboration_events (organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(id), sqlc.arg(sequence), sqlc.arg(type), sqlc.arg(schema_version), sqlc.arg(actor_kind), sqlc.arg(actor_principal_id), sqlc.arg(event_revision), sqlc.arg(change_id), sqlc.arg(version_id), sqlc.arg(head_sha), sqlc.arg(run_id), sqlc.arg(attempt_id), sqlc.arg(from_state), sqlc.arg(to_state), sqlc.arg(recorded_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes)) RETURNING *;

-- name: RecordListEvent :many
SELECT * FROM collaboration_events WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND sequence>sqlc.arg(after) ORDER BY sequence LIMIT sqlc.arg(page_limit);

-- name: RecordGetCollaborationVersion :one
SELECT * FROM collaboration_versions WHERE organization_id=sqlc.arg(organization_id) AND record_id=sqlc.arg(record_id) AND revision=sqlc.arg(revision);

-- name: RecordInsertCollaborationVersion :one
INSERT INTO collaboration_versions (organization_id, project_id, work_item_id, record_id, revision, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(record_id), sqlc.arg(revision), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes)) RETURNING *;

-- name: RecordGetChange :one
SELECT * FROM change_requests WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id);

-- name: RecordInsertChange :one
INSERT INTO change_requests (organization_id, project_id, work_item_id, id, revision, title, current_version_id, evidence_sequence, created_at, updated_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(id), sqlc.arg(revision), sqlc.arg(title), sqlc.narg(current_version_id), sqlc.arg(evidence_sequence), sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes)) RETURNING *;

-- name: RecordListChange :many
SELECT * FROM change_requests WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND id>sqlc.arg(after) ORDER BY id LIMIT sqlc.arg(page_limit);

-- name: RecordGetLink :one
SELECT * FROM change_issue_links WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND work_item_id=sqlc.arg(work_item_id);

-- name: RecordInsertLink :one
INSERT INTO change_issue_links (organization_id, project_id, work_item_id, change_id)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(change_id)) RETURNING *;

-- name: RecordGetVersion :one
SELECT * FROM change_versions WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND id=sqlc.arg(id);

-- name: RecordInsertVersion :one
INSERT INTO change_versions (organization_id, id, change_id, number, head_sha, base_sha, merge_base_sha, repository, policy_revision_id, attempt_id, actor_kind, actor_principal_id, external_provider, external_id, external_url, created_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(id), sqlc.arg(change_id), sqlc.arg(number), sqlc.arg(head_sha), sqlc.arg(base_sha), sqlc.arg(merge_base_sha), sqlc.arg(repository), sqlc.arg(policy_revision_id), sqlc.arg(attempt_id), sqlc.arg(actor_kind), sqlc.arg(actor_principal_id), sqlc.arg(external_provider), sqlc.arg(external_id), sqlc.arg(external_url), sqlc.arg(created_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes)) RETURNING *;

-- name: RecordListVersion :many
SELECT * FROM change_versions WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND number>sqlc.arg(after) ORDER BY number LIMIT sqlc.arg(page_limit);

-- name: RecordGetEvidence :one
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND kind=sqlc.arg(kind) AND source_key=sqlc.arg(source_key);

-- name: RecordInsertEvidence :one
INSERT INTO change_evidence (organization_id, change_id, sequence, version_id, kind, source_key, actor_kind, actor_principal_id, decision, created_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(change_id), sqlc.arg(sequence), sqlc.narg(version_id), sqlc.arg(kind), sqlc.narg(source_key), sqlc.arg(actor_kind), sqlc.arg(actor_principal_id), sqlc.arg(decision), sqlc.arg(created_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes)) RETURNING *;

-- name: RecordListEvidence :many
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND sequence>sqlc.arg(after) ORDER BY sequence LIMIT sqlc.arg(page_limit);

-- name: RecordLockIssue :one
SELECT event_sequence FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR UPDATE;

-- name: RecordAdvanceIssue :one
UPDATE issues SET event_sequence=greatest(event_sequence, sqlc.arg(imported_sequence)::bigint) + CASE WHEN sqlc.arg(imported_sequence)::bigint = 0 THEN 1 ELSE 0 END
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND native_id=sqlc.arg(native_id)
RETURNING event_sequence;

-- name: RecordLockChange :one
SELECT * FROM change_requests WHERE organization_id=$1 AND id=$2 FOR UPDATE;

-- name: RecordAdvanceEvidence :one
UPDATE change_requests SET evidence_sequence=greatest(evidence_sequence, sqlc.arg(imported_sequence)::bigint) + CASE WHEN sqlc.arg(imported_sequence)::bigint = 0 THEN 1 ELSE 0 END
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) RETURNING evidence_sequence;

-- name: RecordListLink :many
SELECT * FROM change_issue_links WHERE organization_id=$1 AND change_id=$2 AND work_item_id>sqlc.arg(after) ORDER BY work_item_id LIMIT sqlc.arg(page_limit);

-- name: RecordUpdateChange :one
UPDATE change_requests SET revision=revision+1, title=sqlc.arg(title), current_version_id=sqlc.narg(current_version_id),
updated_at=sqlc.arg(updated_at), payload_object_key=sqlc.arg(payload_object_key), payload_sha256=sqlc.arg(payload_sha256),
payload_bytes=sqlc.arg(payload_bytes), payload_content_bytes=sqlc.arg(payload_content_bytes)
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND revision=sqlc.arg(expected_revision)
RETURNING *;

-- name: RecordGetEvidenceSequence :one
SELECT * FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=$3;

-- name: RecordGetReceipt :one
SELECT * FROM native_commands WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4;

-- name: RecordInsertReceipt :one
INSERT INTO native_commands (organization_id,actor_id,operation,command_key,request_hash,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (organization_id,actor_id,operation,command_key) DO NOTHING RETURNING *;
