-- name: DuplicateSourceState :one
SELECT (p.duplicate_source_sequence+p.content_settings_revision)::bigint AS source_sequence,
(NOT EXISTS (
 SELECT 1 FROM search_postings WHERE organization_id=p.organization_id
 AND scope=public.search_scope_key(p.id,'','work','','','all','') AND term='~pending'
) AND NOT EXISTS (
 SELECT 1 FROM search_scope_refresh WHERE organization_id=p.organization_id AND project_id=p.id
))::boolean AS ready
FROM projects p WHERE p.organization_id=sqlc.arg(organization_id) AND p.id=sqlc.arg(project_id) AND p.deleted_at IS NULL;

-- name: DuplicateSources :many
WITH hits AS MATERIALIZED (
 SELECT DISTINCT ON (d.work_item_id) d.id,ts_rank(d.candidate_vector,sqlc.arg(expression)::tsquery) AS rank,i.number,(d.candidate_vector @@ sqlc.arg(fingerprint_expression)::tsquery) AS fingerprint_hit
 FROM (
 SELECT d.*,d.vector AS candidate_vector FROM search_documents d WHERE d.kind='issue' AND NOT d.placeholder AND d.indexed_revision=d.revision AND (d.vector || array_to_tsvector(ARRAY['~'||md5(d.organization_id||':'||md5(d.id))])) @@ sqlc.arg(expression)::tsquery
 UNION ALL
 SELECT d.*,d.source_vector AS candidate_vector FROM search_documents d WHERE d.kind='comment' AND NOT d.placeholder AND d.source_vector @@ sqlc.arg(expression)::tsquery
) d
LEFT JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id
LEFT JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
WHERE d.organization_id=sqlc.arg(organization_id) AND d.project_id=sqlc.arg(project_id)
AND d.kind IN ('issue','comment') AND NOT d.placeholder
AND d.candidate_vector @@ sqlc.arg(expression)::tsquery
AND ((sqlc.arg(exact)::boolean AND d.candidate_vector @@ sqlc.arg(literal_expression)::tsquery) OR (NOT sqlc.arg(exact)::boolean AND (d.candidate_vector @@ sqlc.arg(fingerprint_expression)::tsquery
 OR (SELECT count(*) FROM unnest(sqlc.arg(title_expressions)::text[]) term WHERE d.candidate_vector @@ term::tsquery)>=sqlc.arg(title_minimum)::bigint
 OR (SELECT count(*) FROM unnest(sqlc.arg(body_expressions)::text[]) term WHERE d.candidate_vector @@ term::tsquery)>=sqlc.arg(body_minimum)::bigint)))
AND ((d.kind='issue' AND d.source_id=i.native_id AND d.revision=i.revision) OR (d.kind='comment' AND EXISTS(SELECT 1 FROM native_comments c WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id) AND c.organization_id=d.organization_id AND c.project_id=d.project_id AND c.id=d.source_id AND c.work_item_id=d.work_item_id AND c.revision=d.revision AND NOT c.deleted)))
AND i.native_id IS NOT NULL
AND coalesce((NOT i.archived AND (NOT sqlc.arg(reporter)::boolean OR i.actor_principal_id=sqlc.arg(principal_id)::text)
AND (w.name IS NOT NULL AND (sqlc.arg(machine)::boolean OR NOT w.terminal OR coalesce((SELECT e.recorded_at FROM collaboration_events e
JOIN project_workflow_states destination ON destination.organization_id=e.organization_id AND destination.project_id=e.project_id AND destination.name=e.to_state AND destination.terminal
JOIN project_workflow_states origin ON origin.organization_id=e.organization_id AND origin.project_id=e.project_id AND origin.name=e.from_state AND NOT origin.terminal
WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence DESC LIMIT 1),i.native_created_at)>=sqlc.arg(terminal_after)::timestamptz))),false)
ORDER BY d.work_item_id,fingerprint_hit DESC,(d.kind='comment') DESC,rank DESC,i.number DESC,d.id COLLATE "C"
), candidates AS MATERIALIZED (
 SELECT id,rank,number,fingerprint_hit FROM hits ORDER BY fingerprint_hit DESC,rank DESC,number DESC,id COLLATE "C" LIMIT 5
)
SELECT sqlc.embed(d),d.token_count::bigint AS source_tokens,d.terms::text[] AS source_terms,d.term_frequencies::bigint[] AS source_frequencies,
coalesce(i.revision,0)::bigint AS issue_revision,coalesce(i.number,0)::bigint AS issue_number,coalesce(i.workflow_state_name,'')::text AS issue_state,coalesce(w.terminal,false)::boolean AS terminal
FROM candidates hit JOIN search_documents d ON d.organization_id=sqlc.arg(organization_id) AND d.id=hit.id
LEFT JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id
LEFT JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
ORDER BY hit.fingerprint_hit DESC,hit.rank DESC,hit.number DESC,d.id COLLATE "C";

-- name: DuplicateProjectLock :one
SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE;

-- name: DuplicateDocumentIdentity :one
SELECT project_id,kind FROM search_documents WHERE organization_id=$1 AND id=$2;

-- name: DuplicateParentSource :one
SELECT * FROM search_documents WHERE organization_id=$1 AND project_id=$2 AND kind='issue' AND source_id=$3 AND NOT placeholder;


-- name: AdvanceDuplicateSource :execrows
UPDATE projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=$1 AND id=$2;
