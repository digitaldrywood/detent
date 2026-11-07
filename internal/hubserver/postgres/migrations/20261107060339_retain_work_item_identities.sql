-- +goose Up
-- +goose StatementBegin
CREATE TABLE work_item_identities (
    organization_id text NOT NULL,
    id bigint NOT NULL,
    project_id text NOT NULL,
    native_id text NOT NULL,
    PRIMARY KEY (organization_id,id),
    UNIQUE (organization_id,native_id),
    UNIQUE (organization_id,project_id,native_id),
    UNIQUE (organization_id,project_id,id)
);
LOCK TABLE issues IN SHARE ROW EXCLUSIVE MODE;
INSERT INTO work_item_identities SELECT organization_id,id,project_id,native_id FROM issues;
ALTER TABLE work_item_identities OWNER TO detent_migration;
REVOKE ALL ON work_item_identities FROM PUBLIC,detent_runtime,detent_platform;
GRANT SELECT ON work_item_identities TO detent_runtime,detent_platform;
ALTER TABLE work_item_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_item_identities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON work_item_identities TO detent_runtime
    USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON work_item_identities TO detent_platform USING (true);
CREATE POLICY migration_access ON work_item_identities TO detent_migration USING (true) WITH CHECK (true);
CREATE TRIGGER work_item_identities_immutable BEFORE UPDATE OR DELETE ON work_item_identities
    FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();

CREATE FUNCTION capture_work_item_identity() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF TG_RELID<>'public.issues'::regclass OR TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'invalid work item identity owner' USING ERRCODE='23514';
    END IF;
    INSERT INTO public.work_item_identities(organization_id,id,project_id,native_id)
    VALUES(NEW.organization_id,NEW.id,NEW.project_id,NEW.native_id);
    RETURN NEW;
END;
$$;
ALTER FUNCTION capture_work_item_identity() OWNER TO detent_migration;
REVOKE ALL ON FUNCTION capture_work_item_identity() FROM PUBLIC;
CREATE TRIGGER issues_capture_identity BEFORE INSERT ON issues FOR EACH ROW EXECUTE FUNCTION capture_work_item_identity();

CREATE OR REPLACE FUNCTION preserve_issue_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.organization_id,NEW.id,NEW.project_id,NEW.native_id,NEW.created_at)
        IS DISTINCT FROM (OLD.organization_id,OLD.id,OLD.project_id,OLD.native_id,OLD.created_at) THEN
        RAISE EXCEPTION 'work item identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION require_live_work_item() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE skip_check boolean; live_id bigint;
BEGIN
    IF TG_OP='UPDATE' THEN
        EXECUTE 'SELECT '||TG_ARGV[2] INTO skip_check USING NEW,OLD;
        IF skip_check THEN RETURN NEW; END IF;
    END IF;
    EXECUTE 'SELECT '||TG_ARGV[0] INTO skip_check USING NEW;
    IF skip_check THEN RETURN NEW; END IF;
    EXECUTE TG_ARGV[1] INTO live_id USING NEW;
    IF live_id IS NULL THEN
        RAISE EXCEPTION 'work item does not exist' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;

DO $$
DECLARE fk record; definition text; columns text; nulls text; predicates text; unchanged text;
BEGIN
    FOR fk IN SELECT c.*,c.conrelid::regclass AS relation FROM pg_constraint c
        WHERE c.contype='f' AND c.confrelid='issues'::regclass AND c.confdeltype='a'
    LOOP
        definition:=regexp_replace(pg_get_constraintdef(fk.oid),' NOT VALID$','');
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',fk.relation,fk.conname);
        IF fk.relation::text=ANY(ARRAY['native_comments','issue_labels','issue_dependencies','issue_relationships','workspace_items','issue_claim_facts','queue_entries','artifact_upload_authorizations']) THEN
            IF definition ~ ' (NOT )?DEFERRABLE' THEN
                definition:=regexp_replace(definition,'( (NOT )?DEFERRABLE)',' ON DELETE CASCADE\1');
            ELSE definition:=definition||' ON DELETE CASCADE'; END IF;
        ELSE
            definition:=regexp_replace(definition,'REFERENCES (public\.)?issues','REFERENCES public.work_item_identities');
            SELECT string_agg(format('%I',local.attname),',' ORDER BY k.ordinality),
                string_agg(format('($1).%I IS NULL',local.attname),' OR ' ORDER BY k.ordinality),
                string_agg(format('i.%I=($1).%I',remote.attname,local.attname),' AND ' ORDER BY k.ordinality),
                string_agg(format('($1).%I IS NOT DISTINCT FROM ($2).%I',local.attname,local.attname),' AND ' ORDER BY k.ordinality)
            INTO columns,nulls,predicates,unchanged
            FROM unnest(fk.conkey,fk.confkey) WITH ORDINALITY k(local_id,remote_id,ordinality)
            JOIN pg_attribute local ON local.attrelid=fk.conrelid AND local.attnum=k.local_id
            JOIN pg_attribute remote ON remote.attrelid=fk.confrelid AND remote.attnum=k.remote_id;
            EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OF %s ON %s FOR EACH ROW EXECUTE FUNCTION require_live_work_item(%L,%L,%L)',
                'work_item_live_'||substr(md5(fk.conname),1,16),columns,fk.relation,nulls,
                'SELECT i.id FROM public.issues i WHERE '||predicates||' FOR KEY SHARE',unchanged);
        END IF;
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s NOT VALID',fk.relation,fk.conname,definition);
        EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT %I',fk.relation,fk.conname);
    END LOOP;
END;
$$;

CREATE FUNCTION preserve_executing_work_item() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF EXISTS(SELECT 1 FROM public.leases WHERE organization_id=OLD.organization_id AND issue_id=OLD.id AND released_at IS NULL)
        OR EXISTS(SELECT 1 FROM public.native_attempts WHERE organization_id=OLD.organization_id AND issue_id=OLD.id AND status='running') THEN
        RAISE EXCEPTION 'work item execution is active' USING ERRCODE='23503';
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER issues_preserve_execution BEFORE DELETE ON issues FOR EACH ROW EXECUTE FUNCTION preserve_executing_work_item();
CREATE INDEX attachment_item_references_work_item_idx ON attachment_item_references(organization_id,project_id,work_item_id);
CREATE FUNCTION delete_work_item_attachment_references() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF TG_RELID='public.issues'::regclass THEN
        DELETE FROM public.attachment_item_references WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id AND work_item_id=OLD.native_id;
    ELSE
        DELETE FROM public.attachment_item_references WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id AND work_item_id=OLD.work_item_id AND comment_id=OLD.id;
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER issues_delete_attachment_references BEFORE DELETE ON issues FOR EACH ROW EXECUTE FUNCTION delete_work_item_attachment_references();
CREATE TRIGGER comments_delete_attachment_references BEFORE DELETE ON native_comments FOR EACH ROW EXECUTE FUNCTION delete_work_item_attachment_references();

CREATE TRIGGER leases_live_reactivation BEFORE UPDATE OF released_at ON leases
    FOR EACH ROW WHEN (NEW.released_at IS NULL AND OLD.released_at IS NOT NULL)
    EXECUTE FUNCTION require_live_work_item('($1).issue_id IS NULL','SELECT id FROM public.issues WHERE organization_id=($1).organization_id AND id=($1).issue_id FOR KEY SHARE','false');
CREATE TRIGGER native_attempts_live_insert BEFORE INSERT OR UPDATE OF organization_id,issue_id ON native_attempts
    FOR EACH ROW EXECUTE FUNCTION require_live_work_item('($1).issue_id IS NULL','SELECT id FROM public.issues WHERE organization_id=($1).organization_id AND id=($1).issue_id FOR KEY SHARE','($1).organization_id IS NOT DISTINCT FROM ($2).organization_id AND ($1).issue_id IS NOT DISTINCT FROM ($2).issue_id');
CREATE TRIGGER native_attempts_live_running BEFORE UPDATE OF status ON native_attempts
    FOR EACH ROW WHEN (NEW.status='running')
    EXECUTE FUNCTION require_live_work_item('($1).issue_id IS NULL','SELECT id FROM public.issues WHERE organization_id=($1).organization_id AND id=($1).issue_id FOR KEY SHARE','false');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE fk record; trigger_row record; definition text;
BEGIN
    IF EXISTS(SELECT 1 FROM work_item_identities identity LEFT JOIN issues i USING(organization_id,id)
        WHERE i.id IS NULL) THEN
        RAISE EXCEPTION 'retained work item history prevents downgrade' USING ERRCODE='23514';
    END IF;
    FOR trigger_row IN SELECT tgrelid::regclass AS relation,tgname FROM pg_trigger
        WHERE tgfoid='require_live_work_item()'::regprocedure
    LOOP EXECUTE format('DROP TRIGGER %I ON %s',trigger_row.tgname,trigger_row.relation); END LOOP;
    FOR fk IN SELECT c.*,c.conrelid::regclass AS relation FROM pg_constraint c
        WHERE c.contype='f' AND (c.confrelid='work_item_identities'::regclass OR
            (c.confrelid='issues'::regclass AND c.confdeltype='c' AND c.conrelid::regclass::text=ANY(ARRAY['native_comments','issue_labels','issue_dependencies','issue_relationships','workspace_items','issue_claim_facts','queue_entries','artifact_upload_authorizations'])))
    LOOP
        definition:=regexp_replace(pg_get_constraintdef(fk.oid),'REFERENCES (public\.)?work_item_identities','REFERENCES public.issues');
        definition:=replace(definition,' ON DELETE CASCADE','');
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',fk.relation,fk.conname);
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s',fk.relation,fk.conname,definition);
    END LOOP;
END;
$$;
DROP TRIGGER issues_delete_attachment_references ON issues;
DROP TRIGGER comments_delete_attachment_references ON native_comments;
DROP FUNCTION delete_work_item_attachment_references();
DROP INDEX attachment_item_references_work_item_idx;
DROP TRIGGER issues_preserve_execution ON issues;
DROP FUNCTION preserve_executing_work_item();
DROP TRIGGER issues_capture_identity ON issues;
DROP FUNCTION capture_work_item_identity();
DROP FUNCTION require_live_work_item();
DROP TABLE work_item_identities;
CREATE OR REPLACE FUNCTION preserve_issue_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.created_at) THEN
        RAISE EXCEPTION 'work item identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
