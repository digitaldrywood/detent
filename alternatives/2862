DO $roles$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'detent_migration_v2') THEN
        CREATE ROLE detent_migration_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'detent_runtime_v2') THEN
        CREATE ROLE detent_runtime_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'detent_platform_v2') THEN
        CREATE ROLE detent_platform_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname IN ('detent_migration_v2', 'detent_runtime_v2', 'detent_platform_v2')
        AND (rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
    ) THEN
        RAISE EXCEPTION 'Detent group roles must be unprivileged and unable to log in';
    END IF;
    IF pg_has_role('detent_runtime_v2', 'detent_platform_v2', 'MEMBER')
        OR pg_has_role('detent_runtime_v2', 'detent_migration_v2', 'MEMBER')
        OR pg_has_role('detent_platform_v2', 'detent_migration_v2', 'MEMBER')
        OR pg_has_role('detent_platform_v2', 'detent_runtime_v2', 'MEMBER')
        OR pg_has_role('detent_migration_v2', 'detent_runtime_v2', 'MEMBER')
        OR pg_has_role('detent_migration_v2', 'detent_platform_v2', 'MEMBER') THEN
        RAISE EXCEPTION 'Detent database roles must have separate authority';
    END IF;
END
$roles$;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO detent_migration_v2;
GRANT USAGE ON SCHEMA public TO detent_runtime_v2, detent_platform_v2;

CREATE SCHEMA IF NOT EXISTS platform AUTHORIZATION detent_migration_v2;
REVOKE ALL ON SCHEMA platform FROM PUBLIC, detent_runtime_v2;
GRANT USAGE ON SCHEMA platform TO detent_platform_v2;

CREATE OR REPLACE FUNCTION public.search_scope_key(project_id text,work_item_id text,kind text,scope text,status text,archived text,principal_id text)
RETURNS text LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $key$
SELECT encode(sha256(convert_to(encode(convert_to(COALESCE($1,''),'UTF8'),'hex') || ':' || encode(convert_to(COALESCE($2,''),'UTF8'),'hex') || ':' || $3 || ':' || $4 || ':' || $5 || ':' || $6 || ':' || encode(convert_to(COALESCE($7,''),'UTF8'),'hex'),'UTF8')),'hex')
$key$;
CREATE OR REPLACE FUNCTION public.search_scope_keys(project_id text,work_item_id text,kind text,scope text,status text,archived boolean,principal_id text)
RETURNS SETOF text LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $keys$
SELECT DISTINCT public.search_scope_key(p,w,CASE WHEN $3='thread' THEN 'thread' ELSE 'work' END,s,t,a,r)
FROM unnest(CASE WHEN $3='thread' THEN ARRAY['',COALESCE($1,'')] ELSE ARRAY[$1] END) p
CROSS JOIN unnest(ARRAY['',COALESCE($2,'')]) w
CROSS JOIN unnest(ARRAY['',$4]) s
CROSS JOIN unnest(ARRAY['',$5]) t
CROSS JOIN unnest(ARRAY['all',CASE WHEN $6 THEN 'true' ELSE 'false' END]) a
CROSS JOIN unnest(CASE WHEN $3='thread' THEN ARRAY[''] ELSE ARRAY['',COALESCE($7,'')] END) r
$keys$;

DROP FUNCTION IF EXISTS public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint);
DROP FUNCTION IF EXISTS public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[]);
DROP FUNCTION IF EXISTS public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean);
DROP FUNCTION IF EXISTS public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[]);
CREATE OR REPLACE FUNCTION public.search_index_candidates(
    expression tsquery,project_id text,project_ids text[],restrict_projects boolean,
    work_item_id text,kind text,scope text,status text,archived text,reporter boolean,principal_id text,
    before_updated_at timestamptz,before_id text,before_submicrosecond bigint,
    search_terms text[] DEFAULT '{}',exact boolean DEFAULT false,
    stream_keys text[] DEFAULT '{}',stream_ids text[] DEFAULT '{}',stream_impacts double precision[] DEFAULT '{}',
    stream_times text[] DEFAULT '{}',stream_submicroseconds bigint[] DEFAULT '{}',stream_done boolean[] DEFAULT '{}'
) RETURNS TABLE(candidate_id text,scan_boundary boolean,stream_key text,stream_impact double precision,stream_exhausted boolean) LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $search$
BEGIN
 RETURN QUERY
 WITH scopes AS MATERIALIZED (
    SELECT DISTINCT public.search_scope_key(p,$5,$6,CASE WHEN $4 AND p='' THEN 'general' ELSE $7 END,$8,
        CASE WHEN $9='all' THEN 'all' WHEN $9='true' THEN 'true' ELSE 'false' END,CASE WHEN $10 THEN $11 ELSE '' END) AS key,p AS project
    FROM unnest(CASE WHEN $4 THEN array_append($3,'') ELSE ARRAY[$2] END) p
    WHERE ($2='' OR p=$2) AND (NOT $4 OR p<>'' OR $7 IN ('','general'))
 ), requested AS MATERIALIZED (
    SELECT key,term FROM scopes CROSS JOIN unnest(CASE WHEN $1::tsquery IS NULL THEN ARRAY['~pending'] WHEN numnode($1::tsquery)=0 THEN ARRAY['~all'] ELSE $15::text[] END) term
 ), positions AS MATERIALIZED (
    SELECT split_part(c.key,chr(9),1) AS key,split_part(c.key,chr(9),2) AS lexeme,NULLIF(c.id,'') AS id,c.impact,c.at::timestamptz AS at,c.submicrosecond,c.done
    FROM unnest($17::text[],$18::text[],$19::double precision[],$20::text[],$21::bigint[],$22::boolean[]) c(key,id,impact,at,submicrosecond,done)
 ), starts AS MATERIALIZED (
    SELECT r.*,p.first_active,p.last_lexeme FROM requested r LEFT JOIN LATERAL (
        SELECT min(lexeme) FILTER (WHERE NOT done AND id<>'') AS first_active,max(lexeme) AS last_lexeme
        FROM positions WHERE key=r.key||':'||r.term
    ) p ON true
 ), lexemes AS MATERIALIZED (
    SELECT r.key,r.term,l.lexeme,l.ordinal FROM starts r CROSS JOIN LATERAL (
        WITH RECURSIVE walk AS (
            (SELECT p.term AS lexeme,1 AS ordinal FROM (
                (SELECT p.term FROM public.search_postings p
                 WHERE ($16 OR left(r.term,1)='~') AND (r.first_active IS NOT NULL OR r.last_lexeme IS NULL)
                    AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key AND p.term=r.term COLLATE "C"
                 ORDER BY p.term LIMIT 1)
                UNION ALL
                (SELECT p.term FROM public.search_postings p
                 WHERE NOT $16 AND left(r.term,1)<>'~' AND (r.first_active IS NOT NULL OR r.last_lexeme IS NULL)
                    AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key
                    AND p.term>=COALESCE(r.first_active,r.term) COLLATE "C" AND p.term<(r.term||chr(1114111)) COLLATE "C"
                 ORDER BY p.term LIMIT 1)
                UNION ALL
                (SELECT p.term FROM public.search_postings p
                 WHERE NOT $16 AND left(r.term,1)<>'~' AND r.first_active IS NULL AND r.last_lexeme IS NOT NULL
                    AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key
                    AND p.term>r.last_lexeme COLLATE "C" AND p.term<(r.term||chr(1114111)) COLLATE "C"
                 ORDER BY p.term LIMIT 1)
            ) p ORDER BY p.term LIMIT 1)
            UNION ALL
            SELECT p.term,w.ordinal+1 FROM walk w CROSS JOIN LATERAL (
                SELECT p.term FROM public.search_postings p
                WHERE NOT $16 AND left(r.term,1)<>'~' AND w.ordinal<129
                    AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key
                    AND p.term>w.lexeme COLLATE "C" AND p.term<(r.term||chr(1114111)) COLLATE "C"
                ORDER BY p.term LIMIT 1
            ) p
        ) SELECT * FROM walk
    ) l
 ), streams AS MATERIALIZED (
    SELECT l.*,c.id AS after_id,c.impact AS after_impact,c.at AS after_at,c.submicrosecond AS after_submicrosecond,COALESCE(c.done,false) AS done,
        greatest(1,2000/least(128,count(*) OVER (PARTITION BY l.key,l.term))) AS budget,
        EXISTS(SELECT 1 FROM lexemes more WHERE more.key=l.key AND more.term=l.term AND more.ordinal=129) AS more_lexemes
    FROM lexemes l LEFT JOIN positions c ON c.key=l.key||':'||l.term AND c.lexeme=l.lexeme WHERE l.ordinal<=128
 ), windows AS MATERIALIZED (
    SELECT r.*,p.*,row_number() OVER (PARTITION BY r.key,r.term,r.lexeme ORDER BY CASE WHEN $6='thread' THEN p.updated_at END DESC,CASE WHEN $6='thread' THEN p.updated_at_submicrosecond END DESC,CASE WHEN $6='thread' THEN p.document_id END DESC,p.impact DESC,p.document_id COLLATE "C") AS position
    FROM streams r CROSS JOIN LATERAL (
        SELECT p.* FROM (
            (SELECT p.document_id,p.impact,p.updated_at,p.updated_at_submicrosecond FROM public.search_postings p
             WHERE $6='work' AND NOT r.done AND r.after_id IS NULL AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key AND p.term=r.lexeme
                AND ($6<>'thread' OR $13='' OR (p.updated_at,p.updated_at_submicrosecond,p.document_id COLLATE "C")<($12::timestamptz,$14::bigint,$13 COLLATE "C"))
             ORDER BY p.impact DESC,p.document_id COLLATE "C" LIMIT r.budget+1)
            UNION ALL
            (SELECT p.document_id,p.impact,p.updated_at,p.updated_at_submicrosecond FROM public.search_postings p
             WHERE $6='work' AND NOT r.done AND r.after_id IS NOT NULL AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key AND p.term=r.lexeme
                AND p.impact=r.after_impact AND p.document_id COLLATE "C">r.after_id COLLATE "C"
                AND ($6<>'thread' OR $13='' OR (p.updated_at,p.updated_at_submicrosecond,p.document_id COLLATE "C")<($12::timestamptz,$14::bigint,$13 COLLATE "C"))
             ORDER BY p.impact DESC,p.document_id COLLATE "C" LIMIT r.budget+1)
            UNION ALL
            (SELECT p.document_id,p.impact,p.updated_at,p.updated_at_submicrosecond FROM public.search_postings p
             WHERE $6='work' AND NOT r.done AND r.after_id IS NOT NULL AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key AND p.term=r.lexeme AND p.impact<r.after_impact
                AND ($6<>'thread' OR $13='' OR (p.updated_at,p.updated_at_submicrosecond,p.document_id COLLATE "C")<($12::timestamptz,$14::bigint,$13 COLLATE "C"))
             ORDER BY p.impact DESC,p.document_id COLLATE "C" LIMIT r.budget+1)
            UNION ALL
            (SELECT p.document_id,p.impact,p.updated_at,p.updated_at_submicrosecond FROM public.search_postings p
             WHERE $6='thread' AND NOT r.done AND r.after_id IS NULL
                AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key AND p.term=r.lexeme
                AND ($13='' OR (p.updated_at,p.updated_at_submicrosecond,p.document_id COLLATE "C")<($12::timestamptz,$14::bigint,$13 COLLATE "C"))
             ORDER BY p.updated_at DESC,p.updated_at_submicrosecond DESC,p.document_id COLLATE "C" DESC LIMIT r.budget+1)
            UNION ALL
            (SELECT p.document_id,p.impact,p.updated_at,p.updated_at_submicrosecond FROM public.search_postings p
             WHERE $6='thread' AND NOT r.done AND r.after_id IS NOT NULL
                AND p.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND p.scope=r.key AND p.term=r.lexeme
                AND (p.updated_at,p.updated_at_submicrosecond,p.document_id COLLATE "C")<(r.after_at,r.after_submicrosecond,r.after_id COLLATE "C")
             ORDER BY p.updated_at DESC,p.updated_at_submicrosecond DESC,p.document_id COLLATE "C" DESC LIMIT r.budget+1)
        ) p ORDER BY CASE WHEN $6='thread' THEN p.updated_at END DESC,CASE WHEN $6='thread' THEN p.updated_at_submicrosecond END DESC,
            CASE WHEN $6='thread' THEN p.document_id END DESC,p.impact DESC,p.document_id COLLATE "C" LIMIT r.budget+1
    ) p
 ), window_sizes AS MATERIALIZED (
    SELECT key,term,lexeme,max(position)<=max(budget) AS exhausted FROM windows GROUP BY key,term,lexeme
 ), stream_boundaries AS MATERIALIZED (
    SELECT DISTINCT ON (w.key,w.term,w.lexeme) w.key,w.term,w.lexeme,w.document_id,w.impact,w.updated_at,w.updated_at_submicrosecond,w.more_lexemes,s.exhausted
    FROM windows w JOIN window_sizes s ON s.key=w.key AND s.term=w.term AND s.lexeme=w.lexeme
    WHERE w.position<=w.budget ORDER BY w.key,w.term,w.lexeme,w.position DESC
 ), postings AS MATERIALIZED (
    SELECT w.key,w.document_id,w.term AS requested,w.lexeme,w.impact FROM windows w WHERE w.position<=w.budget
 ), refreshing AS MATERIALIZED (
    SELECT q.* FROM scopes s CROSS JOIN unnest(CASE WHEN $9='all' THEN ARRAY[false,true] ELSE ARRAY[$9='true'] END) a(archived)
    CROSS JOIN LATERAL (
      (SELECT q.* FROM public.search_scope_refresh q
       WHERE $5='' AND NOT $10 AND s.project<>'' AND q.organization_id=NULLIF(current_setting('detent.organization_id',true),'')
         AND q.project_id=s.project AND q.archived=a.archived AND ($5='' OR q.work_item_id=$5)
       ORDER BY q.refresh_order LIMIT greatest(1,1000/greatest(1,(SELECT count(*)::int FROM scopes))))
      UNION ALL
      (SELECT q.* FROM public.search_scope_refresh q
       WHERE $5='' AND $10 AND s.project<>'' AND q.organization_id=NULLIF(current_setting('detent.organization_id',true),'')
         AND q.project_id=s.project AND q.principal_id=$11 AND q.archived=a.archived AND ($5='' OR q.work_item_id=$5)
       ORDER BY q.refresh_order LIMIT greatest(1,1000/greatest(1,(SELECT count(*)::int FROM scopes))))
      UNION ALL
      (SELECT q.* FROM public.search_scope_refresh q
       WHERE $5='' AND NOT $10 AND s.project='' AND NOT $4 AND q.organization_id=NULLIF(current_setting('detent.organization_id',true),'')
         AND q.archived=a.archived AND ($5='' OR q.work_item_id=$5)
       ORDER BY q.refresh_order LIMIT 1000)
      UNION ALL
      (SELECT q.* FROM public.search_scope_refresh q
       WHERE $5<>'' AND q.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND q.work_item_id=$5 AND q.work_item_id<>''
         AND (s.project='' OR q.project_id=s.project) AND q.archived=a.archived AND (NOT $10 OR q.principal_id=$11) LIMIT 1)
    ) q
 ), refreshing_documents AS MATERIALIZED (
    SELECT d.id FROM refreshing q CROSS JOIN LATERAL (
        (SELECT d.id FROM public.search_scope_documents d WHERE q.work_item_id<>'' AND d.organization_id=q.organization_id
         AND d.project_id=q.project_id AND d.work_item_id=q.work_item_id AND d.id COLLATE "C">q.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1)
        UNION ALL
        (SELECT d.id FROM public.search_scope_documents d WHERE q.work_item_id='' AND d.organization_id=q.organization_id
         AND d.project_id=q.project_id AND d.work_item_id='' AND d.id COLLATE "C">q.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1)
    ) d
 ), coverage AS MATERIALIZED (
    SELECT document_id AS id,count(DISTINCT requested) FILTER (WHERE impact>0) AS coverage FROM postings GROUP BY document_id
 ), batches AS MATERIALIZED (
    SELECT key,requested,string_agg(quote_literal('~'||md5(NULLIF(current_setting('detent.organization_id',true),'')||':'||md5(document_id))),' | ')::tsquery AS identities
    FROM postings GROUP BY key,requested
 ), indexed AS MATERIALIZED (
    SELECT d.id FROM batches b CROSS JOIN LATERAL (
        SELECT d.id FROM public.search_documents d
        WHERE d.organization_id=NULLIF(current_setting('detent.organization_id',true),'')
            AND (d.vector || array_to_tsvector(ARRAY['~'||md5(d.organization_id||':'||md5(d.id))])) @@ b.identities
        LIMIT 2000
    ) d
 ), ids AS MATERIALIZED (
    SELECT id,max(coverage) AS coverage FROM (
        SELECT c.id,c.coverage FROM coverage c JOIN indexed i ON i.id=c.id
        UNION ALL SELECT id,coverage FROM coverage WHERE $1::tsquery IS NULL OR numnode($1::tsquery)=0
        UNION ALL SELECT id,0 FROM refreshing_documents
    ) matches GROUP BY id
 ), scoped AS NOT MATERIALIZED (
    SELECT d.*,ids.coverage FROM ids CROSS JOIN LATERAL (SELECT d.* FROM public.search_documents d WHERE d.organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND d.id=ids.id OFFSET 0) d
    WHERE ($6<>'thread' OR $13='' OR (d.updated_at,d.updated_at_submicrosecond,d.id COLLATE "C")<($12::timestamptz,$14::bigint,$13 COLLATE "C"))
        AND ($2='' OR d.project_id=$2) AND (NOT $4 OR d.project_id=ANY($3) OR d.scope='general')
        AND ($5='' OR d.work_item_id=$5)
        AND (($6='thread' AND d.kind='thread') OR ($6='work' AND d.kind='issue'))
        AND ($7='' OR d.scope=$7) AND ($8='' OR d.status=$8)
        AND (d.project_id IS NULL OR COALESCE((SELECT p.deleted_at IS NULL FROM public.projects p WHERE p.organization_id=d.organization_id AND p.id=d.project_id),false))
        AND (d.work_item_id IS NULL OR COALESCE((SELECT ($9='all' OR i.archived=($9='true')) AND (NOT $10 OR i.actor_principal_id=$11)
            FROM public.issues i WHERE i.organization_id=d.organization_id AND i.project_id=d.project_id AND i.native_id=d.work_item_id),false))
        AND (d.kind<>'comment' OR COALESCE((SELECT NOT c.deleted FROM public.native_comments c WHERE c.organization_id=d.organization_id AND c.id=d.source_id),false))
 ), owners AS MATERIALIZED (
    SELECT d.id,owner.key,owner.term,owner.lexeme FROM scoped d CROSS JOIN LATERAL (
      SELECT r.key,r.term,p.term AS lexeme FROM requested r CROSS JOIN LATERAL (
       SELECT p.term,p.impact FROM public.search_postings p
       WHERE p.organization_id=d.organization_id AND p.document_id=d.id AND p.scope=r.key
        AND (($16 OR left(r.term,1)='~') AND p.term=r.term COLLATE "C"
          OR NOT $16 AND left(r.term,1)<>'~' AND p.term>=r.term COLLATE "C" AND p.term<(r.term||chr(1114111)) COLLATE "C")
       ORDER BY p.impact DESC,p.term COLLATE "C" LIMIT 1
      ) p CROSS JOIN LATERAL (
       SELECT front.document_id,front.impact FROM public.search_postings front
       WHERE front.organization_id=d.organization_id AND front.scope=r.key AND front.term=p.term
       ORDER BY front.impact DESC,front.document_id COLLATE "C" LIMIT 1
      ) front
      ORDER BY (left(split_part(r.term,'_',2),1)='w') DESC,(front.document_id=d.id) DESC,
       p.impact/greatest(front.impact,0.000001) DESC,r.term DESC,p.term COLLATE "C" LIMIT 1
    ) owner WHERE $6='work'
 ), owned_ids AS MATERIALIZED (
    SELECT o.id FROM owners o JOIN postings p ON p.key=o.key AND p.requested=o.term AND p.lexeme=o.lexeme AND p.document_id=o.id
    UNION SELECT d.id FROM scoped d JOIN refreshing_documents f ON f.id=d.id WHERE cardinality($17::text[])=0 AND NOT EXISTS(SELECT 1 FROM owners o WHERE o.id=d.id)
 )

 (SELECT d.id,false,''::text,0::double precision,false FROM scoped d
 WHERE ($6<>'work' OR d.id IN (SELECT id FROM owned_ids)
    OR $1::tsquery IS NULL AND d.source_vector=''::tsvector AND d.vector=''::tsvector)
 ORDER BY CASE WHEN $6='work' THEN d.coverage END DESC,
    CASE WHEN $6='work' THEN ts_rank(d.vector,$1::tsquery) END DESC,
    d.updated_at DESC,d.updated_at_submicrosecond DESC,d.id COLLATE "C" DESC)
 UNION ALL SELECT b.document_id,true,b.key||':'||b.term||chr(9)||b.lexeme||CASE WHEN b.exhausted AND b.more_lexemes THEN chr(9)||'advance' ELSE '' END,b.impact,b.exhausted AND NOT b.more_lexemes FROM stream_boundaries b;
END
$search$;
ALTER FUNCTION public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[]) OWNER TO detent_platform_v2;
REVOKE ALL ON FUNCTION public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[]) TO detent_runtime_v2;
