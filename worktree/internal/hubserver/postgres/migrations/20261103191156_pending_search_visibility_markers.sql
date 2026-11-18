-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
DECLARE parent_archived boolean:=false; parent_principal text:=''; hidden boolean:=false; task public.search_scope_refresh; next_id text; visibility_scopes text[];
BEGIN
 IF NEW.kind NOT IN ('issue','thread') THEN RETURN NEW; END IF;
 IF NEW.project_id IS NOT NULL THEN
  INSERT INTO public.search_scope_documents(organization_id,project_id,work_item_id,id)
   SELECT DISTINCT NEW.organization_id,NEW.project_id,item,NEW.id FROM unnest(ARRAY['',COALESCE(NEW.work_item_id,'')]) item ON CONFLICT DO NOTHING;
 END IF;
 IF NEW.project_id IS NOT NULL THEN
  SELECT deleted_at IS NOT NULL INTO hidden FROM public.projects WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 END IF;
 IF NEW.work_item_id IS NOT NULL THEN
  SELECT archived,COALESCE(actor_principal_id,'') INTO parent_archived,parent_principal FROM public.issues
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND native_id=NEW.work_item_id;
 END IF;
 IF NEW.indexed_revision<>NEW.revision THEN
  IF NOT hidden THEN
   SELECT array_agg(key) INTO visibility_scopes
   FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key;
  END IF;
  DELETE FROM public.search_postings
  WHERE organization_id=NEW.organization_id AND document_id=NEW.id AND term IN ('~all','~pending')
   AND (hidden OR NOT (scope=ANY(visibility_scopes)));
  IF NOT hidden THEN
   INSERT INTO public.search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
   SELECT NEW.organization_id,key,term,NEW.id,0,NEW.updated_at,NEW.updated_at_submicrosecond
   FROM unnest(visibility_scopes) key CROSS JOIN unnest(ARRAY['~all','~pending']) term
   ON CONFLICT(organization_id,scope,term,document_id) DO UPDATE
   SET impact=EXCLUDED.impact,updated_at=EXCLUDED.updated_at,updated_at_submicrosecond=EXCLUDED.updated_at_submicrosecond
   WHERE (search_postings.impact,search_postings.updated_at,search_postings.updated_at_submicrosecond)
    IS DISTINCT FROM (EXCLUDED.impact,EXCLUDED.updated_at,EXCLUDED.updated_at_submicrosecond);
  END IF;
 ELSE
  DELETE FROM public.search_postings WHERE organization_id=NEW.organization_id AND document_id=NEW.id;
 IF NOT hidden THEN
  INSERT INTO public.search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
  WITH prefixes AS (
   SELECT DISTINCT 's'||encode(convert_to(NEW.organization_id,'UTF8'),'hex')||'z'||encode(convert_to(p,'UTF8'),'hex')||'_' AS prefix
   FROM unnest(ARRAY['',COALESCE(NEW.project_id,'')]) p
  ), words AS (
   SELECT prefix||'w'||term AS lexeme,frequency::double precision/(1+NEW.token_count) AS impact
   FROM prefixes CROSS JOIN unnest(NEW.terms,NEW.term_frequencies) AS t(term,frequency)
  ), lexemes AS (
   SELECT term,COALESCE(w.impact,0) AS impact FROM (
    SELECT unnest(tsvector_to_array(NEW.source_vector)) AS term UNION SELECT unnest(tsvector_to_array(NEW.vector)) UNION SELECT lexeme FROM words
   ) terms LEFT JOIN words w ON w.lexeme=term
   UNION ALL SELECT '~all',0
   UNION ALL SELECT '~pending',0 WHERE NEW.indexed_revision<>NEW.revision
  )
  SELECT NEW.organization_id,key,l.term,NEW.id,l.impact,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key CROSS JOIN lexemes l
  WHERE NEW.indexed_revision=NEW.revision OR l.term IN ('~all','~pending')
    OR NEW.kind<>'thread' AND key=public.search_scope_key(NEW.project_id,NEW.work_item_id,'work','','','all','');
 END IF;
 END IF;
 IF NEW.indexed_revision=NEW.revision OR (TG_OP='UPDATE' AND NEW.refresh_order<>OLD.refresh_order AND NEW.indexed_revision=0) THEN
  FOR task IN SELECT q.* FROM public.search_scope_refresh q WHERE q.organization_id=NEW.organization_id AND q.project_id=NEW.project_id AND q.work_item_id IN ('',COALESCE(NEW.work_item_id,'')) FOR UPDATE LOOP
   IF task.work_item_id='' THEN
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   ELSE
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   END IF;
   IF next_id=NEW.id THEN
    UPDATE public.search_scope_refresh SET after_id=NEW.id,refresh_order=nextval('public.search_refresh_order') WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    IF task.work_item_id='' THEN
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    ELSE
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    END IF;
    IF next_id IS NULL THEN
     DELETE FROM public.search_scope_refresh WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    END IF;
   END IF;
  END LOOP;
 END IF;
 RETURN NEW;
END
$postings$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION search_document_postings() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $postings$
DECLARE parent_archived boolean:=false; parent_principal text:=''; hidden boolean:=false; task public.search_scope_refresh; next_id text;
BEGIN
 IF NEW.kind NOT IN ('issue','thread') THEN RETURN NEW; END IF;
 IF NEW.project_id IS NOT NULL THEN
  INSERT INTO public.search_scope_documents(organization_id,project_id,work_item_id,id)
   SELECT DISTINCT NEW.organization_id,NEW.project_id,item,NEW.id FROM unnest(ARRAY['',COALESCE(NEW.work_item_id,'')]) item ON CONFLICT DO NOTHING;
 END IF;
 DELETE FROM public.search_postings WHERE organization_id=NEW.organization_id AND document_id=NEW.id;
 IF NEW.project_id IS NOT NULL THEN
  SELECT deleted_at IS NOT NULL INTO hidden FROM public.projects WHERE organization_id=NEW.organization_id AND id=NEW.project_id;
 END IF;
 IF NEW.work_item_id IS NOT NULL THEN
  SELECT archived,COALESCE(actor_principal_id,'') INTO parent_archived,parent_principal FROM public.issues
   WHERE organization_id=NEW.organization_id AND project_id=NEW.project_id AND native_id=NEW.work_item_id;
 END IF;
 IF NOT hidden THEN
  INSERT INTO public.search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond)
  WITH prefixes AS (
   SELECT DISTINCT 's'||encode(convert_to(NEW.organization_id,'UTF8'),'hex')||'z'||encode(convert_to(p,'UTF8'),'hex')||'_' AS prefix
   FROM unnest(ARRAY['',COALESCE(NEW.project_id,'')]) p
  ), words AS (
   SELECT prefix||'w'||term AS lexeme,frequency::double precision/(1+NEW.token_count) AS impact
   FROM prefixes CROSS JOIN unnest(NEW.terms,NEW.term_frequencies) AS t(term,frequency)
  ), lexemes AS (
   SELECT term,COALESCE(w.impact,0) AS impact FROM (
    SELECT unnest(tsvector_to_array(NEW.source_vector)) AS term UNION SELECT unnest(tsvector_to_array(NEW.vector)) UNION SELECT lexeme FROM words
   ) terms LEFT JOIN words w ON w.lexeme=term
   UNION ALL SELECT '~all',0
   UNION ALL SELECT '~pending',0 WHERE NEW.indexed_revision<>NEW.revision
  )
  SELECT NEW.organization_id,key,l.term,NEW.id,l.impact,NEW.updated_at,NEW.updated_at_submicrosecond
  FROM public.search_scope_keys(NEW.project_id,NEW.work_item_id,NEW.kind,NEW.scope,NEW.status,parent_archived,parent_principal) key CROSS JOIN lexemes l
  WHERE NEW.indexed_revision=NEW.revision OR l.term IN ('~all','~pending')
    OR NEW.kind<>'thread' AND key=public.search_scope_key(NEW.project_id,NEW.work_item_id,'work','','','all','');
 END IF;
 IF NEW.indexed_revision=NEW.revision OR (TG_OP='UPDATE' AND NEW.refresh_order<>OLD.refresh_order AND NEW.indexed_revision=0) THEN
  FOR task IN SELECT q.* FROM public.search_scope_refresh q WHERE q.organization_id=NEW.organization_id AND q.project_id=NEW.project_id AND q.work_item_id IN ('',COALESCE(NEW.work_item_id,'')) FOR UPDATE LOOP
   IF task.work_item_id='' THEN
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   ELSE
    SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">task.after_id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
   END IF;
   IF next_id=NEW.id THEN
    UPDATE public.search_scope_refresh SET after_id=NEW.id,refresh_order=nextval('public.search_refresh_order') WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    IF task.work_item_id='' THEN
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id='' AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    ELSE
     SELECT d.id INTO next_id FROM public.search_scope_documents d WHERE d.organization_id=task.organization_id AND d.project_id=task.project_id AND d.work_item_id=task.work_item_id AND d.id COLLATE "C">NEW.id COLLATE "C" ORDER BY d.id COLLATE "C" LIMIT 1;
    END IF;
    IF next_id IS NULL THEN
     DELETE FROM public.search_scope_refresh WHERE organization_id=task.organization_id AND project_id=task.project_id AND work_item_id=task.work_item_id;
    END IF;
   END IF;
  END LOOP;
 END IF;
 RETURN NEW;
END
$postings$;
-- +goose StatementEnd
