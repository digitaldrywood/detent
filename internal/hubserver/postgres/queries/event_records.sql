-- name: RecordGetEvent :one
SELECT * FROM collaboration_events WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND id=sqlc.arg(id);

-- name: RecordInsertEvent :one
INSERT INTO collaboration_events (organization_id, project_id, work_item_id, id, sequence, project_sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(id), sqlc.arg(sequence), sqlc.arg(project_sequence), sqlc.arg(type), sqlc.arg(schema_version), sqlc.arg(actor_kind), sqlc.arg(actor_principal_id), sqlc.arg(event_revision), sqlc.arg(change_id), sqlc.arg(version_id), sqlc.arg(head_sha), sqlc.arg(landing_waiting), sqlc.arg(landing_change_id), sqlc.arg(landing_version_id), sqlc.arg(landing_head_sha), sqlc.arg(run_id), sqlc.arg(attempt_id), sqlc.arg(from_state), sqlc.arg(to_state), sqlc.arg(recorded_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes), sqlc.arg(payload_format_version), sqlc.arg(push_known), sqlc.arg(push_change_id), sqlc.arg(push_version_id), sqlc.arg(push_head_sha), sqlc.arg(push_external_effect), sqlc.arg(push_effect_state)) RETURNING *;

-- name: RecordAdvanceProjectEventSequence :one
INSERT INTO project_event_cursors (organization_id, project_id, sequence)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), 1)
ON CONFLICT (organization_id, project_id) DO UPDATE SET sequence=project_event_cursors.sequence+1
RETURNING sequence;

-- name: RecordProjectEventSequence :one
SELECT COALESCE((SELECT sequence FROM project_event_cursors WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)),0)::bigint;

-- name: RecordProjectChanges :many
WITH changed AS (
    SELECT work_item_id FROM (
        SELECT work_item_id
        FROM collaboration_events
        WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
          AND project_sequence>sqlc.arg(after) AND project_sequence<=sqlc.arg(through)
        ORDER BY project_sequence
        LIMIT sqlc.arg(page_limit)
    ) events
    UNION
    SELECT native_id FROM (
        SELECT i.native_id
        FROM issues i
        WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
          AND i.last_activity_at>sqlc.arg(since)
        ORDER BY i.last_activity_at,i.native_id
        LIMIT sqlc.arg(page_limit)
    ) activity
    ORDER BY work_item_id
    LIMIT sqlc.arg(page_limit)
)
SELECT i.native_id, i.revision, i.last_activity_at
FROM changed
JOIN issues i ON i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=changed.work_item_id
ORDER BY i.native_id;

-- name: RecordListEvent :many
SELECT * FROM collaboration_events WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND sequence>sqlc.arg(after) ORDER BY sequence LIMIT sqlc.arg(page_limit);

-- name: RecordListEventBefore :many
SELECT * FROM collaboration_events WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND sequence<sqlc.arg(before) ORDER BY sequence DESC LIMIT sqlc.arg(page_limit);

-- name: RecordLatestRunEvent :one
SELECT * FROM collaboration_events
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
AND attempt_id=sqlc.arg(attempt_id)
ORDER BY sequence DESC LIMIT 1;

-- name: RecordLatestCheckpointEvents :many
SELECT latest.*
FROM unnest(sqlc.arg(attempt_ids)::text[]) AS requested(attempt_id)
CROSS JOIN LATERAL (
  SELECT * FROM collaboration_events
  WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
    AND work_item_id=sqlc.arg(work_item_id) AND attempt_id=requested.attempt_id
    AND type='run.checkpointed'
  ORDER BY sequence DESC LIMIT 1
) latest;

-- name: RecordGetCollaborationVersion :one
SELECT * FROM collaboration_versions WHERE organization_id=sqlc.arg(organization_id) AND record_id=sqlc.arg(record_id) AND revision=sqlc.arg(revision);

-- name: RecordInsertCollaborationVersion :one
INSERT INTO collaboration_versions (organization_id, project_id, work_item_id, record_id, revision, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(record_id), sqlc.arg(revision), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes)) RETURNING *;

-- name: RecordGetChange :one
SELECT * FROM change_requests WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id);

-- name: RecordLatestChange :one
SELECT c.* FROM change_issue_links l
JOIN change_requests c ON c.organization_id=l.organization_id AND c.project_id=l.project_id AND c.id=l.change_id
WHERE l.organization_id=sqlc.arg(organization_id) AND l.project_id=sqlc.arg(project_id) AND l.work_item_id=sqlc.arg(work_item_id)
ORDER BY l.created_at DESC,l.change_id DESC LIMIT 1;

-- name: RecordLatestChangesByWorkItems :many
SELECT requested.work_item_id::text AS linked_work_item_id, c.*
FROM unnest(sqlc.arg(work_item_ids)::text[]) AS requested(work_item_id)
CROSS JOIN LATERAL (
  SELECT c.* FROM change_issue_links l
  JOIN change_requests c ON c.organization_id=l.organization_id AND c.project_id=l.project_id AND c.id=l.change_id
  WHERE l.organization_id=sqlc.arg(organization_id) AND l.project_id=sqlc.arg(project_id)
    AND l.work_item_id=requested.work_item_id
  ORDER BY l.created_at DESC,l.change_id DESC
  LIMIT 1
) c;

-- name: RecordInsertChange :one
INSERT INTO change_requests (organization_id, project_id, work_item_id, id, revision, title, current_version_id, evidence_sequence, created_at, updated_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, landed_version_id, landed_at, landed_head_sha, landed_merge_sha, landed_base_ref)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(work_item_id), sqlc.arg(id), sqlc.arg(revision), sqlc.arg(title), sqlc.narg(current_version_id), sqlc.arg(evidence_sequence), sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes), sqlc.narg(landed_version_id), sqlc.narg(landed_at), sqlc.narg(landed_head_sha), sqlc.narg(landed_merge_sha), sqlc.narg(landed_base_ref)) RETURNING *;

-- name: RecordListChange :many
SELECT c.* FROM change_requests c WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id) AND c.id>sqlc.arg(after)
AND EXISTS (SELECT 1 FROM change_issue_links l WHERE l.organization_id=c.organization_id AND l.project_id=c.project_id AND l.change_id=c.id AND l.work_item_id=sqlc.arg(work_item_id))
ORDER BY c.id LIMIT sqlc.arg(page_limit);

-- name: RecordGetLink :one
SELECT * FROM change_issue_links WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND work_item_id=sqlc.arg(work_item_id);

-- name: RecordInsertLink :one
INSERT INTO change_issue_links (organization_id, project_id, work_item_id, change_id, created_at)
SELECT c.organization_id,c.project_id,sqlc.arg(work_item_id),c.id,c.created_at FROM change_requests c
WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id) AND c.id=sqlc.arg(change_id) RETURNING *;

-- name: RecordGetVersion :one
SELECT * FROM change_versions WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND id=sqlc.arg(id);

-- name: RecordInsertVersion :one
INSERT INTO change_versions (organization_id, id, change_id, number, head_sha, base_sha, merge_base_sha, repository, policy_revision_id, attempt_id, actor_kind, actor_principal_id, external_provider, external_id, external_url, created_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, source_object_key, source_sha256, source_bytes, source_content_bytes, implementing_session_id, implementing_machine_id)
VALUES (sqlc.arg(organization_id), sqlc.arg(id), sqlc.arg(change_id), sqlc.arg(number), sqlc.arg(head_sha), sqlc.arg(base_sha), sqlc.arg(merge_base_sha), sqlc.arg(repository), sqlc.arg(policy_revision_id), sqlc.arg(attempt_id), sqlc.arg(actor_kind), sqlc.arg(actor_principal_id), sqlc.arg(external_provider), sqlc.arg(external_id), sqlc.arg(external_url), sqlc.arg(created_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes), sqlc.arg(payload_format_version), sqlc.narg(source_object_key), sqlc.narg(source_sha256), sqlc.narg(source_bytes), sqlc.narg(source_content_bytes), sqlc.arg(implementing_session_id), sqlc.arg(implementing_machine_id)) RETURNING *;

-- name: RecordListVersion :many
SELECT * FROM change_versions WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND number>sqlc.arg(after) ORDER BY number LIMIT sqlc.arg(page_limit);

-- name: RecordGetVersionByID :one
SELECT * FROM change_versions WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND id=sqlc.arg(id);

-- name: RecordGetVersionAttemptID :one
SELECT attempt_id FROM change_versions WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id);

-- name: RecordGetLandingReceipt :one
SELECT * FROM change_landing_receipts WHERE organization_id=sqlc.arg(organization_id) AND attempt_id=sqlc.arg(attempt_id);

-- name: RecordGetLatestLandingReceiptForWorkItem :one
SELECT r.*
FROM change_landing_receipts r
JOIN native_attempts a ON a.organization_id=r.organization_id AND a.id=r.attempt_id
JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
WHERE r.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=sqlc.arg(work_item_id)
ORDER BY a.fencing_token DESC
LIMIT 1;

-- name: RecordInsertLandingReceipt :one
INSERT INTO change_landing_receipts (organization_id, attempt_id, version_id, merge_sha, repository, base_ref, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes)
VALUES (sqlc.arg(organization_id), sqlc.arg(attempt_id), sqlc.arg(version_id), sqlc.arg(merge_sha), sqlc.arg(repository), sqlc.arg(base_ref), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes))
ON CONFLICT (organization_id, attempt_id) DO NOTHING RETURNING *;

-- name: RecordProjectPolicyLanding :one
SELECT v.change_id,v.external_url,v.actor_principal_id
FROM change_landing_receipts l
JOIN change_versions v ON v.organization_id=l.organization_id AND v.id=l.version_id
JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id
WHERE l.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id)
  AND l.merge_sha=sqlc.arg(merge_sha) AND lower(l.repository)=lower(sqlc.arg(repository))
  AND l.base_ref=sqlc.arg(base_ref)
ORDER BY c.id,v.number DESC
LIMIT 1;

-- name: RecordListViewedFiles :many
SELECT * FROM change_viewed_files
WHERE organization_id=sqlc.arg(organization_id) AND version_id=sqlc.arg(version_id) AND principal_id=sqlc.arg(principal_id)
ORDER BY manifest_sha256,file_sha256 LIMIT sqlc.arg(page_limit);

-- name: RecordSetViewedFile :one
INSERT INTO change_viewed_files (organization_id, project_id, change_id, version_id, principal_id, manifest_sha256, file_sha256, viewed, updated_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(project_id), sqlc.arg(change_id), sqlc.arg(version_id), sqlc.arg(principal_id), sqlc.arg(manifest_sha256), sqlc.arg(file_sha256), sqlc.arg(viewed), sqlc.arg(updated_at))
ON CONFLICT (organization_id, version_id, principal_id, manifest_sha256, file_sha256)
DO UPDATE SET viewed=EXCLUDED.viewed, updated_at=EXCLUDED.updated_at
RETURNING *;

-- name: RecordGetEvidence :one
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND kind=sqlc.arg(kind) AND source_key=sqlc.arg(source_key);

-- name: RecordGetEvidenceBySource :one
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND kind=sqlc.arg(kind) AND source_key=sqlc.arg(source_key);

-- name: RecordInsertEvidence :one
INSERT INTO change_evidence (organization_id, change_id, sequence, version_id, kind, source_key, actor_kind, actor_principal_id, decision, created_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version,review_kind,review_landing,review_session_known,review_session_id,review_session_machine)
VALUES (sqlc.arg(organization_id), sqlc.arg(change_id), sqlc.arg(sequence), sqlc.narg(version_id), sqlc.arg(kind), sqlc.narg(source_key), sqlc.arg(actor_kind), sqlc.arg(actor_principal_id), sqlc.arg(decision), sqlc.arg(created_at), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes), sqlc.arg(payload_format_version),sqlc.arg(review_kind),sqlc.arg(review_landing),sqlc.arg(review_session_known),sqlc.arg(review_session_id),sqlc.arg(review_session_machine)) RETURNING *;

-- name: RecordListEvidence :many
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND sequence>sqlc.arg(after) ORDER BY sequence LIMIT sqlc.arg(page_limit);

-- name: RecordLockIssue :one
SELECT event_sequence FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR UPDATE;

-- name: RecordAdvanceIssue :one
UPDATE issues SET last_activity_at=greatest(last_activity_at,sqlc.arg(activity_at)::timestamptz), event_sequence=greatest(event_sequence, sqlc.arg(imported_sequence)::bigint) + CASE WHEN sqlc.arg(imported_sequence)::bigint = 0 THEN 1 ELSE 0 END
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
payload_bytes=sqlc.arg(payload_bytes), payload_content_bytes=sqlc.arg(payload_content_bytes), landed_version_id=sqlc.narg(landed_version_id),
landed_at=sqlc.narg(landed_at), landed_head_sha=sqlc.narg(landed_head_sha), landed_merge_sha=sqlc.narg(landed_merge_sha), landed_base_ref=sqlc.narg(landed_base_ref)
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

-- name: RecordGetLinkedIssueSource :one
SELECT * FROM linked_issue_sources WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id);

-- name: RecordGetLinkedIssueSourcesByWorkItems :many
SELECT * FROM linked_issue_sources
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
  AND work_item_id=ANY(sqlc.arg(work_item_ids)::text[]);

-- name: RecordLatestRunEventsByAttempts :many
SELECT latest.*
FROM unnest(sqlc.arg(attempt_ids)::text[]) AS requested(attempt_id)
CROSS JOIN LATERAL (
  SELECT * FROM collaboration_events
  WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
    AND attempt_id=requested.attempt_id
    AND attempt_id<>''
  ORDER BY sequence DESC
  LIMIT 1
) latest;

-- name: RecordGetLinkedIssueSourceByURL :one
SELECT * FROM linked_issue_sources WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND source_url=sqlc.arg(source_url);

-- name: RecordInsertLinkedIssueSource :one
INSERT INTO linked_issue_sources (organization_id,project_id,work_item_id,source_url,title_supplied,body_supplied,title_owned,body_owned,snapshot_object_key,snapshot_sha256,snapshot_bytes,snapshot_content_bytes)
VALUES (sqlc.arg(organization_id),sqlc.arg(project_id),sqlc.arg(work_item_id),sqlc.arg(source_url),sqlc.arg(title_supplied),sqlc.arg(body_supplied),sqlc.arg(title_owned),sqlc.arg(body_owned),sqlc.narg(snapshot_object_key),sqlc.narg(snapshot_sha256),sqlc.narg(snapshot_bytes),sqlc.narg(snapshot_content_bytes)) RETURNING *;

-- name: RecordUpdateLinkedIssueSource :one
UPDATE linked_issue_sources SET title_owned=sqlc.arg(title_owned),body_owned=sqlc.arg(body_owned),snapshot_object_key=sqlc.narg(snapshot_object_key),snapshot_sha256=sqlc.narg(snapshot_sha256),snapshot_bytes=sqlc.narg(snapshot_bytes),snapshot_content_bytes=sqlc.narg(snapshot_content_bytes)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND source_url=sqlc.arg(source_url) AND snapshot_sha256 IS NOT DISTINCT FROM sqlc.narg(expected_snapshot_sha256)
RETURNING *;

-- name: RecordGetCollaborationVersions :many
SELECT v.* FROM ROWS FROM(unnest(sqlc.arg(record_ids)::text[]),unnest(sqlc.arg(revisions)::bigint[])) input(record_id,revision)
JOIN collaboration_versions v ON v.organization_id=sqlc.arg(organization_id) AND v.record_id=input.record_id AND v.revision=input.revision;

-- name: RecordChangeSummaryEvidence :many
WITH scoped AS NOT MATERIALIZED (
 SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id)
 AND version_id=sqlc.arg(version_id) AND sequence<=sqlc.arg(through) AND kind IN ('review','check')
), human_latest AS (
 SELECT DISTINCT ON (actor_principal_id) * FROM scoped WHERE review_kind='human' AND decision<>'commented'
 ORDER BY actor_principal_id,sequence DESC
), human_decisions AS (
 SELECT DISTINCT ON (decision) * FROM human_latest WHERE decision IN ('approved','changes_requested') ORDER BY decision,sequence DESC
), recovery_actors AS (
 SELECT DISTINCT actor_principal_id FROM scoped WHERE kind='review' AND review_kind='' LIMIT 201
), recovery_humans AS (
 SELECT * FROM human_latest WHERE actor_principal_id IN (SELECT actor_principal_id FROM recovery_actors)
), machine_latest AS (
 SELECT DISTINCT ON (review_kind) * FROM scoped WHERE review_kind IN ('validator','implementation','security_audit') AND NOT review_landing
 ORDER BY review_kind,sequence DESC
), audit_block AS (
 SELECT * FROM scoped WHERE review_kind='security_audit' AND decision='changes_requested' ORDER BY sequence DESC LIMIT 1
), other AS (
 SELECT * FROM scoped WHERE kind='review' AND review_kind='' ORDER BY sequence LIMIT 201
), checks AS (
 SELECT * FROM scoped WHERE kind='check' AND source_key=ANY(sqlc.arg(check_ids)::text[])
)
SELECT * FROM (
 SELECT * FROM human_decisions UNION SELECT * FROM recovery_humans UNION SELECT * FROM machine_latest UNION SELECT * FROM audit_block UNION SELECT * FROM other UNION SELECT * FROM checks
) selected ORDER BY sequence;

-- name: RecordChangePriorApproval :one
SELECT EXISTS (SELECT 1 FROM change_evidence e
WHERE e.organization_id=sqlc.arg(organization_id) AND e.change_id=sqlc.arg(change_id)
AND e.version_id<>sqlc.arg(version_id) AND e.sequence<=sqlc.arg(through)
AND e.kind='review' AND e.review_kind='human' AND e.decision='approved') AS approved,
EXISTS (SELECT 1 FROM change_evidence e
WHERE e.organization_id=sqlc.arg(organization_id) AND e.change_id=sqlc.arg(change_id)
AND e.version_id<>sqlc.arg(version_id) AND e.sequence<=sqlc.arg(through)
AND e.kind='review' AND e.review_kind='' AND e.decision='approved') AS unprojected;

-- name: RecordChangePublicationCheckpoints :many
WITH matching AS (
 SELECT e.* FROM collaboration_events e WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id)
 AND e.work_item_id=sqlc.arg(work_item_id) AND e.type='run.checkpointed'
 AND e.push_change_id=sqlc.arg(change_id) AND e.push_version_id=sqlc.arg(version_id) AND e.push_head_sha=sqlc.arg(head_sha)
 AND e.push_external_effect='git_push' AND e.push_effect_state='confirmed'
 ORDER BY e.sequence DESC LIMIT 1
), unprojected AS (
 SELECT e.* FROM collaboration_events e WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id)
 AND e.work_item_id=sqlc.arg(work_item_id) AND e.type='run.checkpointed' AND NOT e.push_known
 AND e.sequence>COALESCE((SELECT sequence FROM matching),0) ORDER BY e.sequence DESC LIMIT 201
)
SELECT e.* FROM collaboration_events e
JOIN (SELECT sequence FROM matching UNION ALL SELECT sequence FROM unprojected) candidates ON candidates.sequence=e.sequence
WHERE e.organization_id=sqlc.arg(organization_id) AND e.project_id=sqlc.arg(project_id) AND e.work_item_id=sqlc.arg(work_item_id)
ORDER BY e.sequence DESC;

-- name: RecordChangePublicationProvenance :one
SELECT EXISTS (SELECT 1 FROM native_attempts a
JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id AND l.fencing_token=a.fencing_token AND l.issue_id=a.issue_id
JOIN lease_policy_bindings p ON p.organization_id=a.organization_id AND p.lease_id=a.lease_id AND p.project_id=sqlc.arg(project_id) AND p.policy_id=sqlc.arg(policy_id)
JOIN native_attempt_events ae ON ae.organization_id=a.organization_id AND ae.attempt_id=a.id AND ae.sequence=sqlc.arg(run_sequence)
WHERE a.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
AND i.native_id=sqlc.arg(work_item_id) AND a.id=sqlc.arg(attempt_id)
AND a.run_id=sqlc.arg(run_id) AND a.lease_id=sqlc.arg(lease_id)
AND a.fencing_token=sqlc.arg(fencing_token));

-- name: RecordUnprojectedChangeReviews :many
SELECT * FROM change_evidence WHERE organization_id=$1 AND kind='review' AND (review_kind='' OR NOT review_session_known)
ORDER BY change_id,sequence LIMIT $2;

-- name: RecordValidatorReviewEvidence :many
WITH scoped AS NOT MATERIALIZED (
 SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id)
 AND version_id=sqlc.arg(version_id) AND sequence<=sqlc.arg(through) AND kind='review'
), implementation AS (
 SELECT * FROM scoped WHERE review_kind='implementation' AND NOT review_landing ORDER BY sequence DESC LIMIT 1
), reuse AS (
 SELECT e.* FROM (VALUES ('security_audit'),('implementation'),('validator')) k(review_kind)
 CROSS JOIN LATERAL (
  SELECT * FROM scoped WHERE review_session_known AND review_kind=k.review_kind
  AND review_session_machine=sqlc.arg(machine_id) AND review_session_id=sqlc.arg(session_id)
  ORDER BY sequence DESC LIMIT 1
 ) e
), unknown AS (
 SELECT * FROM scoped WHERE review_kind='' OR NOT review_session_known ORDER BY sequence LIMIT 1
)
SELECT * FROM (SELECT * FROM implementation UNION SELECT * FROM reuse UNION SELECT * FROM unknown) selected ORDER BY sequence;

-- name: RecordUnprojectedChangePushes :many
SELECT * FROM collaboration_events WHERE organization_id=$1 AND type='run.checkpointed' AND NOT push_known
ORDER BY project_id,work_item_id,sequence DESC LIMIT $2;
