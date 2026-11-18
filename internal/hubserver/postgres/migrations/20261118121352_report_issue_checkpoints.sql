-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE report_checkpoint_cursors (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 through_project_sequence bigint NOT NULL DEFAULT 0 CHECK (through_project_sequence>=0),
 legacy_work_item_id text NOT NULL DEFAULT '',
 legacy_sequence bigint NOT NULL DEFAULT 0 CHECK (legacy_sequence>=0),
 legacy_complete boolean NOT NULL DEFAULT false,
 PRIMARY KEY (organization_id,project_id),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects (organization_id,id) ON DELETE CASCADE
);
CREATE TABLE report_issue_checkpoints (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 landing_revision bigint NOT NULL DEFAULT 0 CHECK (landing_revision>=0 AND landing_revision<=revision),
 PRIMARY KEY (organization_id,project_id,work_item_id),
 FOREIGN KEY (organization_id,project_id,work_item_id) REFERENCES issues (organization_id,project_id,native_id) ON DELETE CASCADE
);
CREATE INDEX report_issue_checkpoints_landing_due ON report_issue_checkpoints (organization_id,project_id,work_item_id) WHERE landing_revision<revision;
CREATE TABLE report_issue_state_entries (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 state text NOT NULL CHECK (state<>''),
 first_entered_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id,state),
 FOREIGN KEY (organization_id,project_id,work_item_id) REFERENCES issues (organization_id,project_id,native_id) ON DELETE CASCADE
);
CREATE TABLE report_landing_attempts (
 organization_id text NOT NULL,
 change_id text NOT NULL,
 landed_at timestamptz NOT NULL,
 attempts_observed integer NOT NULL CHECK (attempts_observed>=0),
 success_position integer CHECK (success_position>0),
 success_attempts integer CHECK (success_attempts>0),
 success_complete boolean NOT NULL,
 last_attempt_started_at timestamptz,
 last_attempt_id text NOT NULL DEFAULT '',
 PRIMARY KEY (organization_id,change_id),
 FOREIGN KEY (organization_id,change_id) REFERENCES change_requests (organization_id,id) ON DELETE CASCADE,
 CHECK ((success_position IS NULL)=(success_attempts IS NULL)),
 CHECK ((last_attempt_started_at IS NULL)=(last_attempt_id=''))
);
ALTER TABLE report_checkpoint_cursors OWNER TO detent_migration;
ALTER TABLE report_issue_checkpoints OWNER TO detent_migration;
ALTER TABLE report_issue_state_entries OWNER TO detent_migration;
ALTER TABLE report_landing_attempts OWNER TO detent_migration;
REVOKE ALL ON report_checkpoint_cursors,report_issue_checkpoints,report_issue_state_entries,report_landing_attempts FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON report_checkpoint_cursors,report_issue_checkpoints,report_issue_state_entries,report_landing_attempts TO detent_runtime;
ALTER TABLE report_checkpoint_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_checkpoint_cursors FORCE ROW LEVEL SECURITY;
ALTER TABLE report_issue_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_issue_checkpoints FORCE ROW LEVEL SECURITY;
ALTER TABLE report_issue_state_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_issue_state_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE report_landing_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_landing_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON report_checkpoint_cursors TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON report_issue_checkpoints TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON report_issue_state_entries TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY tenant_isolation ON report_landing_attempts TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose StatementBegin
CREATE FUNCTION report_checkpoint_sources() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org text; previous_scope text:=current_setting('detent.organization_id',true);
BEGIN
 IF TG_RELID='public.collaboration_events'::regclass AND TG_OP='INSERT' AND TG_LEVEL='ROW' THEN
  PERFORM set_config('detent.organization_id',NEW.organization_id,true);
  UPDATE public.report_checkpoint_cursors c SET legacy_complete=false,legacy_work_item_id=NEW.work_item_id,legacy_sequence=NEW.sequence-1
  WHERE c.organization_id=NEW.organization_id AND c.project_id=NEW.project_id AND (c.legacy_complete OR (NEW.work_item_id,NEW.sequence-1)<(c.legacy_work_item_id,c.legacy_sequence));
 ELSIF TG_RELID='public.native_attempts'::regclass AND TG_OP='INSERT' AND TG_LEVEL='STATEMENT' THEN
  FOR org IN SELECT DISTINCT organization_id FROM report_checkpoint_rows ORDER BY organization_id LOOP
   PERFORM set_config('detent.organization_id',org,true);
   UPDATE public.report_issue_checkpoints k SET revision=k.revision+1
   FROM (SELECT DISTINCT i.organization_id,i.project_id,i.native_id FROM report_checkpoint_rows n JOIN public.issues i ON i.organization_id=n.organization_id AND i.id=n.issue_id WHERE n.organization_id=org) s
   WHERE k.organization_id=s.organization_id AND k.project_id=s.project_id AND k.work_item_id=s.native_id;
  END LOOP;
 ELSIF TG_RELID='public.report_event_facts'::regclass AND TG_OP='INSERT' AND TG_LEVEL='STATEMENT' THEN
  FOR org IN SELECT DISTINCT organization_id FROM report_checkpoint_rows ORDER BY organization_id LOOP
   PERFORM set_config('detent.organization_id',org,true);
   UPDATE public.report_issue_checkpoints k SET revision=k.revision+1
   FROM (SELECT DISTINCT e.organization_id,e.project_id,e.work_item_id FROM report_checkpoint_rows f JOIN public.collaboration_events e ON e.organization_id=f.organization_id AND e.id=f.event_id WHERE f.organization_id=org AND e.attempt_id<>'') s
   WHERE k.organization_id=s.organization_id AND k.project_id=s.project_id AND k.work_item_id=s.work_item_id;
  END LOOP;
 ELSE
  RAISE EXCEPTION 'invalid report checkpoint source';
 END IF;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RETURN NULL;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION report_checkpoint_sources() FROM PUBLIC;
CREATE TRIGGER report_checkpoint_legacy_events AFTER INSERT ON collaboration_events
FOR EACH ROW WHEN (NEW.project_sequence=0) EXECUTE FUNCTION report_checkpoint_sources();
CREATE TRIGGER report_checkpoint_attempts AFTER INSERT ON native_attempts
REFERENCING NEW TABLE AS report_checkpoint_rows FOR EACH STATEMENT EXECUTE FUNCTION report_checkpoint_sources();
CREATE TRIGGER report_checkpoint_event_facts AFTER INSERT ON report_event_facts
REFERENCING NEW TABLE AS report_checkpoint_rows FOR EACH STATEMENT EXECUTE FUNCTION report_checkpoint_sources();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER report_checkpoint_event_facts ON report_event_facts;
DROP TRIGGER report_checkpoint_attempts ON native_attempts;
DROP TRIGGER report_checkpoint_legacy_events ON collaboration_events;
DROP FUNCTION report_checkpoint_sources();
DROP TABLE report_landing_attempts;
DROP TABLE report_issue_state_entries;
DROP TABLE report_issue_checkpoints;
DROP TABLE report_checkpoint_cursors;
