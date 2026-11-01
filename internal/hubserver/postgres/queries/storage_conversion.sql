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
