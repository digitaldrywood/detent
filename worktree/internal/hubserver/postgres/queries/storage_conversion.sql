-- name: ConversionBounds :one
SELECT record_id, record_sequence FROM (
    (SELECT id AS record_id, sequence AS record_sequence FROM collaboration_events e
     WHERE sqlc.arg(family)::text='runs' AND e.organization_id=sqlc.arg(organization_id)
       AND e.project_id=sqlc.arg(project_id) AND e.work_item_id=sqlc.arg(aggregate_id)
       AND payload_format_version=1
       AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision')
     ORDER BY id DESC, sequence DESC LIMIT 1)
    UNION ALL
    (SELECT r.id AS record_id, r.number AS record_sequence FROM change_versions r
     JOIN change_requests c ON c.organization_id=r.organization_id AND c.id=r.change_id
     WHERE sqlc.arg(family)::text='versions' AND r.organization_id=sqlc.arg(organization_id)
       AND c.project_id=sqlc.arg(project_id) AND r.change_id=sqlc.arg(aggregate_id)
       AND r.payload_format_version=1
     ORDER BY r.id DESC, r.number DESC LIMIT 1)
    UNION ALL
    (SELECT r.change_id AS record_id, r.sequence AS record_sequence FROM change_evidence r
     JOIN change_requests c ON c.organization_id=r.organization_id AND c.id=r.change_id
     WHERE sqlc.arg(family)::text='evidence' AND r.organization_id=sqlc.arg(organization_id)
       AND c.project_id=sqlc.arg(project_id) AND r.change_id=sqlc.arg(aggregate_id)
       AND r.payload_format_version=1
     ORDER BY r.sequence DESC LIMIT 1)
) AS pending;

-- name: ConversionPage :many
SELECT record_id, record_sequence, payload_format_version, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes FROM (
    (SELECT id AS record_id, sequence AS record_sequence, payload_format_version, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes
     FROM collaboration_events e
     WHERE sqlc.arg(family)::text='runs' AND e.organization_id=sqlc.arg(organization_id)
       AND e.project_id=sqlc.arg(project_id) AND e.work_item_id=sqlc.arg(aggregate_id)
       AND payload_format_version=1
       AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision')
       AND (id,sequence)>(sqlc.arg(after_id)::text,sqlc.arg(after_sequence)::bigint)
       AND (id,sequence)<=(sqlc.arg(through_id)::text,sqlc.arg(through_sequence)::bigint)
     ORDER BY id, sequence LIMIT sqlc.arg(page_limit))
    UNION ALL
    (SELECT r.id AS record_id, r.number AS record_sequence, r.payload_format_version, r.payload_object_key, r.payload_sha256, r.payload_bytes, r.payload_content_bytes
     FROM change_versions r JOIN change_requests c ON c.organization_id=r.organization_id AND c.id=r.change_id
     WHERE sqlc.arg(family)::text='versions' AND r.organization_id=sqlc.arg(organization_id)
       AND c.project_id=sqlc.arg(project_id) AND r.change_id=sqlc.arg(aggregate_id)
       AND r.payload_format_version=1
       AND (r.id,r.number)>(sqlc.arg(after_id)::text,sqlc.arg(after_sequence)::bigint)
       AND (r.id,r.number)<=(sqlc.arg(through_id)::text,sqlc.arg(through_sequence)::bigint)
     ORDER BY r.id, r.number LIMIT sqlc.arg(page_limit))
    UNION ALL
    (SELECT r.change_id AS record_id, r.sequence AS record_sequence, r.payload_format_version, r.payload_object_key, r.payload_sha256, r.payload_bytes, r.payload_content_bytes
     FROM change_evidence r JOIN change_requests c ON c.organization_id=r.organization_id AND c.id=r.change_id
     WHERE sqlc.arg(family)::text='evidence' AND r.organization_id=sqlc.arg(organization_id)
       AND c.project_id=sqlc.arg(project_id) AND r.change_id=sqlc.arg(aggregate_id)
       AND r.payload_format_version=1
       AND r.change_id>=sqlc.arg(after_id)::text AND r.change_id<=sqlc.arg(through_id)::text
       AND r.sequence>sqlc.arg(after_sequence)::bigint AND r.sequence<=sqlc.arg(through_sequence)::bigint
     ORDER BY r.sequence LIMIT sqlc.arg(page_limit))
) AS pending ORDER BY record_id, record_sequence LIMIT sqlc.arg(page_limit);

-- name: ConversionCommit :one
SELECT public.convert_history_payload(
    sqlc.arg(organization_id)::text, sqlc.arg(project_id)::text, sqlc.arg(family)::text,
    sqlc.arg(record_id)::text, sqlc.arg(record_sequence)::bigint, sqlc.arg(aggregate_id)::text,
    sqlc.arg(expected_version)::bigint, sqlc.arg(source_key)::text, sqlc.arg(source_digest)::text,
    sqlc.arg(source_bytes)::bigint, sqlc.arg(source_content_bytes)::bigint,
    sqlc.arg(target_key)::text, sqlc.arg(target_digest)::text,
    sqlc.arg(target_bytes)::bigint, sqlc.arg(target_content_bytes)::bigint
)::boolean AS committed;

-- name: ConversionProgress :one
SELECT converted_rows, converted_bytes FROM storage_conversion_progress
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id)
  AND family=sqlc.arg(family) AND aggregate_id=sqlc.arg(aggregate_id);

-- name: ConversionInitializeCursors :exec
INSERT INTO storage_conversion_cursors (organization_id, family)
SELECT sqlc.arg(organization_id)::text, family FROM unnest(ARRAY['runs', 'versions', 'evidence', 'core-work-events']) AS family
ON CONFLICT DO NOTHING;

-- name: ConversionNextCursor :one
SELECT family, turn, positioned, key_1, key_2, key_3, key_4, sequence
FROM storage_conversion_cursors
WHERE organization_id=sqlc.arg(organization_id) AND family=ANY(sqlc.arg(families)::text[])
ORDER BY turn, family LIMIT 1;

-- name: ConversionReserveCursor :exec
UPDATE storage_conversion_cursors AS target SET turn=(SELECT max(c.turn)+1 FROM storage_conversion_cursors c WHERE c.organization_id=sqlc.arg(organization_id))
WHERE target.organization_id=sqlc.arg(organization_id) AND target.family=sqlc.arg(family);

-- name: ConversionRestoreCursor :exec
UPDATE storage_conversion_cursors SET turn=sqlc.arg(turn)
WHERE organization_id=sqlc.arg(organization_id) AND family=sqlc.arg(family);

-- name: ConversionRecordUsage :exec
UPDATE storage_conversion_cursors SET examined_rows=examined_rows+sqlc.arg(rows)::bigint,
 examined_bytes=examined_bytes+sqlc.arg(bytes)::bigint, oversized_rows=oversized_rows+sqlc.arg(oversized)::bigint
WHERE organization_id=sqlc.arg(organization_id) AND family=sqlc.arg(family);

-- name: ConversionAdvanceCursor :exec
UPDATE storage_conversion_cursors SET positioned=sqlc.arg(positioned),key_1=sqlc.arg(key_1),key_2=sqlc.arg(key_2),key_3=sqlc.arg(key_3),key_4=sqlc.arg(key_4),sequence=sqlc.arg(sequence)
WHERE organization_id=sqlc.arg(organization_id) AND family=sqlc.arg(family);

-- name: ConversionNextRun :one
SELECT project_id, work_item_id AS aggregate_id, id AS record_id, sequence AS record_sequence,
 payload_format_version, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes
FROM collaboration_events
WHERE organization_id=sqlc.arg(organization_id) AND payload_format_version=1
 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision')
 AND (project_id,work_item_id,id,sequence)>(sqlc.arg(project_id)::text,sqlc.arg(aggregate_id)::text,sqlc.arg(record_id)::text,sqlc.arg(sequence)::bigint)
ORDER BY project_id,work_item_id,id,sequence LIMIT 1;

-- name: ConversionNextVersion :one
SELECT c.project_id, r.change_id AS aggregate_id, r.id AS record_id, r.number AS record_sequence,
 r.payload_format_version, r.payload_object_key, r.payload_sha256, r.payload_bytes, r.payload_content_bytes
FROM (
 SELECT change_id,id,number,payload_format_version,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,organization_id
 FROM change_versions AS pending
 WHERE pending.organization_id=sqlc.arg(organization_id) AND payload_format_version=1
  AND (change_id,id,number)>(sqlc.arg(aggregate_id)::text,sqlc.arg(record_id)::text,sqlc.arg(sequence)::bigint)
 ORDER BY change_id,id,number LIMIT 1
) r JOIN change_requests c ON c.organization_id=r.organization_id AND c.id=r.change_id;

-- name: ConversionNextEvidence :one
SELECT c.project_id, r.change_id AS aggregate_id, r.change_id AS record_id, r.sequence AS record_sequence,
 r.payload_format_version, r.payload_object_key, r.payload_sha256, r.payload_bytes, r.payload_content_bytes
FROM (
 SELECT change_id,sequence,payload_format_version,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,organization_id
 FROM change_evidence AS pending
 WHERE pending.organization_id=sqlc.arg(organization_id) AND payload_format_version=1
  AND (change_id,sequence)>(sqlc.arg(aggregate_id)::text,sqlc.arg(sequence)::bigint)
 ORDER BY change_id,sequence LIMIT 1
) r JOIN change_requests c ON c.organization_id=r.organization_id AND c.id=r.change_id;

-- name: ConversionNextCoreWorkEvent :one
SELECT i.project_id,e.issue_id,e.source_id,octet_length(e.payload_json)::bigint AS payload_content_bytes
FROM (SELECT pending.issue_id,pending.source_id,pending.organization_id,pending.payload_json FROM core_work_events pending
 WHERE pending.organization_id=sqlc.arg(organization_id) AND pending.payload_json IS NOT NULL AND pending.source_id>sqlc.arg(after_source_id)::bigint
 ORDER BY pending.source_id LIMIT 1) e
JOIN issues i ON i.organization_id=e.organization_id AND i.id=e.issue_id;
