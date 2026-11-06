-- name: RuntimePublished :one
SELECT (sqlc.arg(revision_id)::text='') OR EXISTS(SELECT 1 FROM public.hosted_runtime_revisions r WHERE r.organization_id=$1 AND r.id=sqlc.arg(revision_id) AND (r.local_published OR EXISTS(SELECT 1 FROM public.hosted_runtime_publications p WHERE p.id=r.id)));

-- name: CurrentRuntime :one
SELECT r.* FROM public.hosted_runtime_heads h
JOIN public.hosted_runtime_revisions staged ON staged.organization_id=h.organization_id AND staged.id=h.prepared_id
JOIN public.hosted_runtime_revisions r ON r.organization_id=h.organization_id AND r.id=CASE WHEN staged.local_published OR EXISTS(SELECT 1 FROM public.hosted_runtime_publications p WHERE p.id=h.prepared_id) THEN h.prepared_id ELSE h.active_id END
WHERE h.organization_id=$1;

-- name: LockRuntimeHead :one
SELECT * FROM public.hosted_runtime_heads WHERE organization_id=$1 FOR UPDATE;

-- name: InsertRuntimeRevision :exec
INSERT INTO public.hosted_runtime_revisions(organization_id,id,local_published,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13);

-- name: SetRuntimeHead :exec
INSERT INTO public.hosted_runtime_heads(organization_id,active_id,prepared_id) VALUES($1,$2,$3)
ON CONFLICT(organization_id) DO UPDATE SET active_id=EXCLUDED.active_id,prepared_id=EXCLUDED.prepared_id;

-- name: LockRuntimePublicationTargets :many
SELECT organization_id FROM public.hosted_runtime_heads WHERE organization_id=ANY(sqlc.arg(organizations)::text[]) AND prepared_id=sqlc.arg(revision)::text ORDER BY organization_id FOR SHARE;

-- name: PublishRuntime :exec
INSERT INTO public.hosted_runtime_publications(id) VALUES($1);

-- name: SetEntryRuntime :exec
INSERT INTO platform.runtime_heads(issuer,revision_id,object_key) VALUES($1,$2,$3)
ON CONFLICT(issuer) DO UPDATE SET revision_id=EXCLUDED.revision_id,object_key=EXCLUDED.object_key;

-- name: EntryRuntime :one
SELECT revision_id,object_key FROM platform.runtime_heads WHERE issuer=$1;

-- name: RuntimeTenantPage :many
SELECT id,generation FROM platform.organizations WHERE id>sqlc.arg(after_id)::text ORDER BY id LIMIT sqlc.arg(page_limit)::integer;

-- name: RuntimeAdoptionTarget :one
SELECT o.generation,o.managed,o.state,r.revision_id,r.object_key,COALESCE(h.prepared_id,'')::text AS applied_revision
FROM platform.organizations o
JOIN platform.runtime_heads r ON r.issuer=sqlc.arg(issuer)::text
LEFT JOIN public.hosted_runtime_heads h ON h.organization_id=o.id
WHERE o.id=sqlc.arg(organization_id)::text;
