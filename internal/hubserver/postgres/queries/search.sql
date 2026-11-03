-- name: SearchPut :execrows
INSERT INTO search_documents (
    organization_id, id, project_id, work_item_id, kind, source_id, revision,
    title, scope, status, updated_at, updated_at_submicrosecond, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, source_vector, token_count, terms, term_frequencies
) SELECT sqlc.arg(organization_id), sqlc.arg(id), sqlc.narg(project_id), sqlc.narg(work_item_id),
    sqlc.arg(kind), sqlc.arg(source_id), sqlc.arg(revision), sqlc.arg(title), sqlc.arg(scope), sqlc.arg(status),
    sqlc.arg(updated_at), sqlc.arg(updated_at_submicrosecond), sqlc.arg(payload_object_key), sqlc.arg(payload_sha256), sqlc.arg(payload_bytes), sqlc.arg(payload_content_bytes), sqlc.arg(source_vector)::tsvector, sqlc.arg(token_count), sqlc.arg(terms)::text[], sqlc.arg(term_frequencies)::bigint[]
WHERE sqlc.arg(expected_revision)::bigint = 0
ON CONFLICT (organization_id, id) DO UPDATE SET
    placeholder=false, revision = EXCLUDED.revision, indexed_revision = 0, vector = ''::tsvector, source_vector=EXCLUDED.source_vector, token_count=EXCLUDED.token_count, terms=EXCLUDED.terms, term_frequencies=EXCLUDED.term_frequencies,
    title = EXCLUDED.title, status = EXCLUDED.status, updated_at = EXCLUDED.updated_at, updated_at_submicrosecond = EXCLUDED.updated_at_submicrosecond,
    payload_object_key = EXCLUDED.payload_object_key, payload_sha256 = EXCLUDED.payload_sha256,
    payload_bytes = EXCLUDED.payload_bytes, payload_content_bytes = EXCLUDED.payload_content_bytes
WHERE (search_documents.revision = sqlc.arg(expected_revision) AND search_documents.revision < EXCLUDED.revision OR search_documents.placeholder AND sqlc.arg(expected_revision)=0)
    AND search_documents.project_id IS NOT DISTINCT FROM EXCLUDED.project_id
    AND search_documents.work_item_id IS NOT DISTINCT FROM EXCLUDED.work_item_id
    AND search_documents.kind = EXCLUDED.kind AND search_documents.source_id = EXCLUDED.source_id
    AND search_documents.scope = EXCLUDED.scope;

-- name: SearchUpdate :execrows
UPDATE search_documents SET placeholder=false, revision = sqlc.arg(revision), indexed_revision = 0, vector = ''::tsvector, source_vector=sqlc.arg(source_vector)::tsvector, token_count=sqlc.arg(token_count), terms=sqlc.arg(terms)::text[], term_frequencies=sqlc.arg(term_frequencies)::bigint[],
    title = sqlc.arg(title), status = sqlc.arg(status), updated_at = sqlc.arg(updated_at), updated_at_submicrosecond = sqlc.arg(updated_at_submicrosecond),
    payload_object_key = sqlc.arg(payload_object_key), payload_sha256 = sqlc.arg(payload_sha256),
    payload_bytes = sqlc.arg(payload_bytes), payload_content_bytes = sqlc.arg(payload_content_bytes)
WHERE organization_id = sqlc.arg(organization_id) AND id = sqlc.arg(id)
    AND revision = sqlc.arg(expected_revision) AND revision < sqlc.arg(revision)
    AND project_id IS NOT DISTINCT FROM sqlc.narg(project_id)
    AND work_item_id IS NOT DISTINCT FROM sqlc.narg(work_item_id)
    AND kind = sqlc.arg(kind) AND source_id = sqlc.arg(source_id) AND scope = sqlc.arg(scope);

-- name: SearchDelete :execrows
DELETE FROM search_documents WHERE organization_id = $1 AND id = $2 AND revision = $3;

-- name: SearchPending :one
WITH task AS MATERIALIZED (
 SELECT * FROM search_scope_refresh WHERE organization_id=$1 ORDER BY refresh_order LIMIT 1
), scope_document AS MATERIALIZED (
 SELECT d.id,t.refresh_order FROM task t CROSS JOIN LATERAL (
  (SELECT d.id FROM search_scope_documents d WHERE t.work_item_id<>'' AND d.organization_id=t.organization_id AND d.project_id=t.project_id
   AND d.work_item_id=t.work_item_id AND d.id COLLATE "C">t.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1)
  UNION ALL
  (SELECT d.id FROM search_scope_documents d WHERE t.work_item_id='' AND d.organization_id=t.organization_id AND d.project_id=t.project_id
   AND d.work_item_id='' AND d.id COLLATE "C">t.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1)
 ) d
), source_document AS MATERIALIZED (
 SELECT id,refresh_order FROM search_documents WHERE organization_id=$1 AND indexed_revision<>revision ORDER BY refresh_order,id LIMIT 1
), candidate AS (
 SELECT * FROM scope_document UNION ALL SELECT * FROM source_document ORDER BY refresh_order,id LIMIT 1
)
SELECT d.* FROM candidate c JOIN search_documents d ON d.organization_id=$1 AND d.id=c.id;

-- name: SearchAdvance :exec
UPDATE search_documents SET refresh_order=nextval('search_refresh_order'),indexed_revision=0,vector=''::tsvector
WHERE organization_id=$1 AND id=$2 AND revision=$3;

-- name: SearchPreparePublicationPostings :one
WITH document AS MATERIALIZED (
 SELECT d.*,coalesce(p.deleted_at IS NOT NULL,false) AS project_hidden,
  coalesce(i.archived,false) AS parent_archived,coalesce(i.actor_principal_id,'') AS parent_principal,
  coalesce(p.name,'')::text AS project_name,coalesce(i.number,0)::bigint AS parent_number,coalesce(i.project_id,'')::text AS parent_project,
  CASE WHEN i.native_id IS NULL THEN NULL::text[] ELSE ARRAY[p.name||'#'||i.number,i.project_id||'#'||i.number] END AS metadata_labels
 FROM search_documents d LEFT JOIN projects p ON p.organization_id=d.organization_id AND p.id=d.project_id
 LEFT JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id
 WHERE d.organization_id=sqlc.arg(organization_id) AND d.id=sqlc.arg(id) AND d.revision=sqlc.arg(revision)
  AND d.payload_object_key=sqlc.arg(payload_object_key) AND d.payload_sha256=sqlc.arg(payload_sha256) AND d.generation=sqlc.arg(generation)
), prefixes AS (
 SELECT DISTINCT 's'||encode(convert_to(d.organization_id,'UTF8'),'hex')||'z'||encode(convert_to(p,'UTF8'),'hex')||'_' AS prefix
 FROM document d CROSS JOIN LATERAL unnest(ARRAY['',coalesce(d.project_id,'')]) p
), words AS (
 SELECT prefix||'w'||term AS lexeme,frequency::double precision/(1+sqlc.arg(token_count)::bigint) AS impact
 FROM prefixes CROSS JOIN ROWS FROM(unnest(sqlc.arg(terms)::text[]),unnest(sqlc.arg(term_frequencies)::bigint[])) AS t(term,frequency)
), lexemes AS (
 SELECT term,coalesce(w.impact,0) AS impact FROM (
  SELECT unnest(tsvector_to_array(sqlc.arg(vector)::tsvector || CASE WHEN d.kind='issue' THEN public.search_metadata_vector(d.organization_id,d.project_id,d.metadata_labels) ELSE ''::tsvector END)) AS term FROM document d UNION SELECT lexeme FROM words
 ) terms LEFT JOIN words w ON w.lexeme=term
), visibility AS MATERIALIZED (
 SELECT d.organization_id,d.id,key FROM document d
 CROSS JOIN LATERAL public.search_scope_keys(d.project_id,d.work_item_id,d.kind,d.scope,d.status,d.parent_archived,d.parent_principal) key
 WHERE d.kind IN ('issue','thread') AND NOT d.project_hidden
), literal_terms AS MATERIALIZED (
 SELECT unnest(sqlc.arg(literals)::text[]) AS term
), current_terms AS MATERIALIZED (
 SELECT term FROM lexemes UNION SELECT term FROM literal_terms
), removed AS (
 DELETE FROM search_postings p USING document d
 WHERE p.organization_id=d.organization_id AND p.document_id=d.id AND p.term NOT IN ('~all','~pending')
  AND d.kind IN ('issue','thread')
  AND (NOT EXISTS(SELECT 1 FROM visibility v WHERE v.key=p.scope)
   OR NOT EXISTS(SELECT 1 FROM current_terms t WHERE t.term=p.term))
 RETURNING 1
), inserted AS (
 INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
 SELECT d.organization_id,key,l.term,d.id,l.impact,d.updated_at,d.updated_at_submicrosecond
 FROM document d
 CROSS JOIN LATERAL public.search_scope_keys(d.project_id,d.work_item_id,d.kind,d.scope,d.status,d.parent_archived,d.parent_principal) key
 CROSS JOIN lexemes l
 WHERE d.kind IN ('issue','thread') AND NOT d.project_hidden
 ON CONFLICT(organization_id,scope,term,document_id) DO UPDATE
 SET impact=EXCLUDED.impact,updated_at=EXCLUDED.updated_at,updated_at_submicrosecond=EXCLUDED.updated_at_submicrosecond,
  literal_positions=CASE WHEN search_postings.term IN (SELECT term FROM literal_terms) THEN search_postings.literal_positions ELSE ARRAY[]::bigint[] END
 WHERE (search_postings.impact,search_postings.updated_at,search_postings.updated_at_submicrosecond,search_postings.literal_positions)
  IS DISTINCT FROM (EXCLUDED.impact,EXCLUDED.updated_at,EXCLUDED.updated_at_submicrosecond,
   CASE WHEN search_postings.term IN (SELECT term FROM literal_terms) THEN search_postings.literal_positions ELSE ARRAY[]::bigint[] END)
 RETURNING 1
)
SELECT d.project_hidden::boolean AS project_hidden,d.parent_archived,d.parent_principal,d.project_name,d.parent_number,d.parent_project,
 CASE WHEN d.project_hidden OR d.kind NOT IN ('issue','thread') THEN ARRAY[]::text[]
 ELSE ARRAY(SELECT public.search_scope_keys(d.project_id,d.work_item_id,d.kind,d.scope,d.status,d.parent_archived,d.parent_principal)) END::text[] AS visibility_scopes
FROM document d CROSS JOIN (SELECT count(*) FROM inserted) wrote;

-- name: SearchPublicationVisibility :one
SELECT coalesce(p.hidden,false)::boolean AS project_hidden,
 coalesce(i.archived,false)::boolean AS parent_archived,coalesce(i.principal,'')::text AS parent_principal,
 coalesce(p.name,'')::text AS project_name,coalesce(i.number,0)::bigint AS parent_number,coalesce(i.project_id,'')::text AS parent_project
FROM search_documents d
LEFT JOIN LATERAL (
 SELECT p.id,p.name,p.deleted_at IS NOT NULL AS hidden FROM projects p
 WHERE p.organization_id=d.organization_id AND p.id=d.project_id FOR SHARE
) p ON true
LEFT JOIN LATERAL (
 SELECT i.archived,i.number,i.project_id,coalesce(i.actor_principal_id,'') AS principal FROM issues i
 WHERE i.organization_id=d.organization_id AND i.project_id=p.id AND i.native_id=d.work_item_id FOR SHARE
) i ON true
WHERE d.organization_id=sqlc.arg(organization_id) AND d.id=sqlc.arg(id);

-- name: SearchPublishLiterals :exec
WITH literals AS MATERIALIZED (
    SELECT l.term,l.positions::bigint[] AS positions
    FROM ROWS FROM(unnest(sqlc.arg(literals)::text[]),unnest(sqlc.arg(positions)::text[])) l(term,positions)
)
INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond,literal_positions)
SELECT d.organization_id,p.scope,l.term,d.id,0,d.updated_at,d.updated_at_submicrosecond,l.positions
FROM search_documents d CROSS JOIN literals l
CROSS JOIN LATERAL (
    SELECT unnest(sqlc.arg(scopes)::text[]) AS scope
    EXCEPT
    SELECT previous.scope FROM search_postings previous
 WHERE previous.organization_id=d.organization_id AND previous.document_id=d.id AND previous.term=l.term AND previous.literal_positions=l.positions
  AND previous.updated_at=d.updated_at AND previous.updated_at_submicrosecond=d.updated_at_submicrosecond
) p
WHERE d.organization_id=sqlc.arg(organization_id) AND d.id=sqlc.arg(id) AND d.revision=sqlc.arg(revision)
ON CONFLICT(organization_id,scope,term,document_id) DO UPDATE SET literal_positions=EXCLUDED.literal_positions,
 updated_at=EXCLUDED.updated_at,updated_at_submicrosecond=EXCLUDED.updated_at_submicrosecond
WHERE (search_postings.literal_positions,search_postings.updated_at,search_postings.updated_at_submicrosecond)
 IS DISTINCT FROM (EXCLUDED.literal_positions,EXCLUDED.updated_at,EXCLUDED.updated_at_submicrosecond);

-- name: SearchPublish :execrows
UPDATE search_documents SET vector = sqlc.arg(vector)::tsvector, source_vector=sqlc.arg(vector)::tsvector, indexed_revision = revision,
    aggregate_object_key=sqlc.narg(aggregate_object_key),aggregate_sha256=sqlc.narg(aggregate_sha256),aggregate_bytes=sqlc.narg(aggregate_bytes),aggregate_content_bytes=sqlc.narg(aggregate_content_bytes),
    token_count = sqlc.arg(token_count), terms = sqlc.arg(terms)::text[], term_frequencies = sqlc.arg(term_frequencies)::bigint[]
WHERE organization_id = sqlc.arg(organization_id) AND id = sqlc.arg(id) AND revision = sqlc.arg(revision)
    AND payload_object_key = sqlc.arg(payload_object_key) AND payload_sha256 = sqlc.arg(payload_sha256) AND generation=sqlc.arg(generation)
;

-- name: SearchCandidates :many
WITH scoped AS NOT MATERIALIZED (
    SELECT d.* FROM search_documents d
    WHERE d.organization_id = sqlc.arg(organization_id)
        AND (sqlc.arg(before_id)::text = '' OR (d.updated_at, d.updated_at_submicrosecond, d.id COLLATE "C") < (sqlc.arg(before_updated_at)::timestamptz, sqlc.arg(before_submicrosecond)::bigint, sqlc.arg(before_id) COLLATE "C"))
        AND (sqlc.arg(project_id)::text = '' OR d.project_id = sqlc.arg(project_id))
        AND (NOT sqlc.arg(restrict_projects)::boolean OR d.project_id = ANY(sqlc.arg(project_ids)::text[]) OR d.scope = 'general')
        AND (sqlc.arg(work_item_id)::text = '' OR d.work_item_id = sqlc.arg(work_item_id))
        AND ((sqlc.arg(kind)::text = 'thread' AND d.kind = 'thread') OR (sqlc.arg(kind) = 'work' AND d.kind = 'issue'))
        AND (sqlc.arg(scope)::text = '' OR d.scope = sqlc.arg(scope))
        AND (sqlc.arg(status)::text = '' OR d.status = sqlc.arg(status))
        AND (d.project_id IS NULL OR COALESCE((SELECT p.deleted_at IS NULL FROM projects p WHERE p.organization_id = d.organization_id AND p.id = d.project_id), false))
        AND (d.work_item_id IS NULL OR COALESCE((
            SELECT (sqlc.arg(archived)::text = 'all' OR i.archived = (sqlc.arg(archived) = 'true'))
                AND (NOT sqlc.arg(reporter)::boolean OR i.actor_principal_id = sqlc.arg(principal_id)::text)
            FROM issues i WHERE i.organization_id = d.organization_id AND i.project_id = d.project_id AND i.native_id = d.work_item_id), false))
        AND (d.kind <> 'comment' OR COALESCE((
            SELECT NOT c.deleted FROM native_comments c WHERE c.organization_id = d.organization_id AND c.id = d.source_id), false))
), indexed AS MATERIALIZED (
    SELECT d.*,match.scan_boundary,match.stream_key,match.stream_impact,match.stream_exhausted FROM public.search_index_candidates(sqlc.arg(expression)::tsquery, sqlc.arg(project_id)::text,
        sqlc.arg(project_ids)::text[], sqlc.arg(restrict_projects)::boolean, sqlc.arg(work_item_id)::text,
        sqlc.arg(kind)::text, sqlc.arg(scope)::text, sqlc.arg(status)::text, sqlc.arg(archived)::text,
        sqlc.arg(reporter)::boolean, sqlc.arg(principal_id)::text, sqlc.arg(before_updated_at)::timestamptz, sqlc.arg(before_id)::text, sqlc.arg(before_submicrosecond)::bigint, sqlc.arg(search_terms)::text[],sqlc.arg(exact)::boolean,sqlc.arg(stream_keys)::text[],sqlc.arg(stream_ids)::text[],sqlc.arg(stream_impacts)::double precision[],sqlc.arg(stream_times)::text[],sqlc.arg(stream_submicroseconds)::bigint[],sqlc.arg(stream_done)::boolean[]) AS match(candidate_id,scan_boundary,stream_key,stream_impact,stream_exhausted)
    CROSS JOIN LATERAL (
        SELECT d.* FROM scoped d WHERE NOT match.scan_boundary AND d.id=match.candidate_id
        UNION ALL
        SELECT d.* FROM search_documents d WHERE match.scan_boundary AND d.organization_id=sqlc.arg(organization_id) AND d.id=match.candidate_id
        OFFSET 0
    ) d WHERE NOT sqlc.arg(empty_query)::boolean
), pending AS MATERIALIZED (
    SELECT d.*,match.scan_boundary,match.stream_key,match.stream_impact,match.stream_exhausted FROM public.search_index_candidates(NULL::tsquery,sqlc.arg(project_id)::text,
        sqlc.arg(project_ids)::text[],sqlc.arg(restrict_projects)::boolean,sqlc.arg(work_item_id)::text,
        sqlc.arg(kind)::text,sqlc.arg(scope)::text,sqlc.arg(status)::text,sqlc.arg(archived)::text,
        sqlc.arg(reporter)::boolean,sqlc.arg(principal_id)::text,sqlc.arg(before_updated_at)::timestamptz,sqlc.arg(before_id)::text,sqlc.arg(before_submicrosecond)::bigint,sqlc.arg(search_terms)::text[],sqlc.arg(exact)::boolean,sqlc.arg(stream_keys)::text[],sqlc.arg(stream_ids)::text[],sqlc.arg(stream_impacts)::double precision[],sqlc.arg(stream_times)::text[],sqlc.arg(stream_submicroseconds)::bigint[],sqlc.arg(stream_done)::boolean[]) AS match(candidate_id,scan_boundary,stream_key,stream_impact,stream_exhausted)
    CROSS JOIN LATERAL (
        SELECT d.* FROM scoped d WHERE NOT match.scan_boundary AND d.id=match.candidate_id
        UNION ALL
        SELECT d.* FROM search_documents d WHERE match.scan_boundary AND d.organization_id=sqlc.arg(organization_id) AND d.id=match.candidate_id
        OFFSET 0
    ) d WHERE NOT sqlc.arg(empty_query)
), numbered AS MATERIALIZED (
    SELECT d.*,false AS scan_boundary,''::text AS stream_key,0::double precision AS stream_impact,false AS stream_exhausted FROM scoped d JOIN issues i ON i.organization_id = d.organization_id AND i.project_id = d.project_id AND i.native_id = d.work_item_id
    JOIN projects p ON p.organization_id = i.organization_id AND p.id = i.project_id
    WHERE sqlc.arg(kind) = 'work' AND sqlc.arg(number)::bigint > 0 AND d.kind = 'issue' AND d.source_id=i.native_id
        AND i.organization_id = sqlc.arg(organization_id) AND i.project_id = sqlc.arg(project_id) AND i.number = sqlc.arg(number)
        AND (sqlc.arg(number_project)::text = '' OR translate(i.project_id,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz') = translate(sqlc.arg(number_project),'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz') OR translate(p.name,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz') = translate(sqlc.arg(number_project),'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz')) LIMIT 1
), listed AS MATERIALIZED (
SELECT d.*,match.scan_boundary,match.stream_key,match.stream_impact,match.stream_exhausted FROM public.search_index_candidates(''::tsquery,sqlc.arg(project_id)::text,
        sqlc.arg(project_ids)::text[],sqlc.arg(restrict_projects)::boolean,sqlc.arg(work_item_id)::text,
        sqlc.arg(kind)::text,sqlc.arg(scope)::text,sqlc.arg(status)::text,sqlc.arg(archived)::text,
        sqlc.arg(reporter)::boolean,sqlc.arg(principal_id)::text,sqlc.arg(before_updated_at)::timestamptz,sqlc.arg(before_id)::text,sqlc.arg(before_submicrosecond)::bigint,sqlc.arg(search_terms)::text[],sqlc.arg(exact)::boolean,sqlc.arg(stream_keys)::text[],sqlc.arg(stream_ids)::text[],sqlc.arg(stream_impacts)::double precision[],sqlc.arg(stream_times)::text[],sqlc.arg(stream_submicroseconds)::bigint[],sqlc.arg(stream_done)::boolean[]) AS match(candidate_id,scan_boundary,stream_key,stream_impact,stream_exhausted)
    CROSS JOIN LATERAL (
        SELECT d.* FROM scoped d WHERE NOT match.scan_boundary AND d.id=match.candidate_id
        UNION ALL
        SELECT d.* FROM search_documents d WHERE match.scan_boundary AND d.organization_id=sqlc.arg(organization_id) AND d.id=match.candidate_id
        OFFSET 0
    ) d WHERE sqlc.arg(empty_query)
), candidates AS MATERIALIZED (
    SELECT * FROM indexed UNION SELECT * FROM pending UNION SELECT * FROM numbered UNION SELECT * FROM listed
)
SELECT candidates.organization_id, candidates.id, COALESCE(candidates.project_id, '')::text AS project_id, candidates.work_item_id,
    candidates.kind, candidates.source_id, candidates.revision, candidates.title, candidates.scope, candidates.status, candidates.updated_at, candidates.updated_at_submicrosecond,
    COALESCE(candidates.aggregate_object_key,candidates.payload_object_key)::text AS payload_object_key,
    COALESCE(candidates.aggregate_sha256,candidates.payload_sha256)::text AS payload_sha256,
    COALESCE(candidates.aggregate_bytes,candidates.payload_bytes)::bigint AS payload_bytes,
    COALESCE(candidates.aggregate_content_bytes,candidates.payload_content_bytes)::bigint AS payload_content_bytes,
    candidates.generation,(candidates.placeholder AND candidates.aggregate_object_key IS NULL)::boolean AS placeholder,
    candidates.scan_boundary::boolean AS scan_boundary, candidates.stream_key::text AS stream_key,candidates.stream_impact::double precision AS stream_impact,candidates.stream_exhausted::boolean AS stream_exhausted, candidates.indexed_revision, candidates.token_count, candidates.terms, candidates.term_frequencies,
    COALESCE((SELECT i.number FROM issues i WHERE i.organization_id = candidates.organization_id AND i.project_id = candidates.project_id AND i.native_id = candidates.work_item_id), 0)::bigint AS number,
    COALESCE((SELECT p.name FROM projects p WHERE p.organization_id = candidates.organization_id AND p.id = candidates.project_id), '')::text AS project_name
FROM candidates
WHERE candidates.scan_boundary OR NOT EXISTS (
    SELECT 1 FROM numbered n WHERE n.work_item_id=candidates.work_item_id AND n.id<>candidates.id
);

-- name: SearchSources :many
SELECT d.* FROM search_documents d
WHERE d.organization_id=sqlc.arg(organization_id) AND d.project_id=sqlc.arg(project_id) AND d.work_item_id=sqlc.arg(work_item_id)
 AND d.kind NOT IN ('issue','thread') AND d.source_visible AND d.id COLLATE "C">sqlc.arg(after_id) COLLATE "C"
 AND (d.kind<>'comment' OR EXISTS(SELECT 1 FROM native_comments c WHERE c.organization_id=d.organization_id AND c.id=d.source_id AND NOT c.deleted))
ORDER BY d.id COLLATE "C" LIMIT sqlc.arg(page_size);

-- name: SearchRemovePlaceholder :exec
DELETE FROM search_documents WHERE organization_id=$1 AND project_id=$2 AND kind='issue' AND source_id=$3 AND placeholder AND id<>$4;
