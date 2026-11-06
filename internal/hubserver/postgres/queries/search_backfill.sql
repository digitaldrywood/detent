-- name: SearchBackfillPage :many
SELECT d.id, d.revision, d.generation, d.payload_sha256,
 COALESCE(c.key_1,'')::text AS after_id
FROM (SELECT COALESCE((SELECT sc.key_1 FROM storage_conversion_cursors sc
 WHERE sc.organization_id=sqlc.arg(organization_id) AND sc.family='search'),'') AS key_1) c
CROSS JOIN LATERAL (
 SELECT id,revision,generation,payload_sha256 FROM search_documents
 WHERE organization_id=sqlc.arg(organization_id) AND search_format=0 AND kind IN ('issue','comment','thread')
 AND id COLLATE "C">c.key_1 COLLATE "C"
 ORDER BY id COLLATE "C" LIMIT sqlc.arg(page_limit)::integer
) d;

-- name: SearchBackfillQueue :exec
WITH advanced AS (
 INSERT INTO storage_conversion_cursors (organization_id,family,positioned,key_1,examined_rows)
 SELECT sqlc.arg(organization_id),'search',true,sqlc.arg(id),1
 WHERE sqlc.arg(after_id)::text=''
 OR EXISTS (SELECT 1 FROM storage_conversion_cursors WHERE organization_id=sqlc.arg(organization_id)
  AND family='search' AND key_1=sqlc.arg(after_id))
 ON CONFLICT (organization_id,family) DO UPDATE SET positioned=true,key_1=EXCLUDED.key_1,
 examined_rows=storage_conversion_cursors.examined_rows+1
 WHERE storage_conversion_cursors.key_1=sqlc.arg(after_id)
 RETURNING organization_id
)
UPDATE search_documents SET indexed_revision=0,generation=nextval('search_refresh_order')
WHERE organization_id IN (SELECT organization_id FROM advanced) AND id=sqlc.arg(id)
 AND revision=sqlc.arg(revision) AND generation=sqlc.arg(generation)
 AND payload_sha256=sqlc.arg(payload_sha256) AND search_format=0 AND indexed_revision=revision;

-- name: SearchBackfillWrap :exec
UPDATE storage_conversion_cursors sc SET positioned=false,key_1=''
WHERE sc.organization_id=sqlc.arg(organization_id) AND sc.family='search'
 AND NOT EXISTS (SELECT 1 FROM search_documents d WHERE d.organization_id=sqlc.arg(organization_id)
  AND d.search_format=0 AND kind IN ('issue','comment','thread')
  AND d.id COLLATE "C">sc.key_1 COLLATE "C");
