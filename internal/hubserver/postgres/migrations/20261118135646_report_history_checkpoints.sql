-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE report_checkpoint_cursors ADD COLUMN history_policy_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE report_issue_checkpoints
 ADD COLUMN history_policy_revision bigint NOT NULL DEFAULT 0,
 ADD COLUMN history_sequence bigint NOT NULL DEFAULT -1,
 ADD COLUMN history_through bigint NOT NULL DEFAULT 0;
CREATE TABLE report_history_checkpoints (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 through_sequence bigint NOT NULL CHECK (through_sequence>0),
 definition_key text NOT NULL,
 created_at timestamptz NOT NULL,
 initial_state text NOT NULL,
 through_recorded_at timestamptz NOT NULL,
 segment_events integer NOT NULL CHECK (segment_events>=0),
 segment_min_at timestamptz,
 segment_max_at timestamptz,
 segment_unclassified boolean NOT NULL,
 reopen_ids text[] NOT NULL,
 reopen_versions text[] NOT NULL,
 reopen_times timestamptz[] NOT NULL,
 reopen_from text[] NOT NULL,
 reopen_to text[] NOT NULL,
 CHECK (cardinality(reopen_ids)<=256 AND cardinality(reopen_ids)=cardinality(reopen_versions) AND cardinality(reopen_ids)=cardinality(reopen_times) AND cardinality(reopen_ids)=cardinality(reopen_from) AND cardinality(reopen_ids)=cardinality(reopen_to)),
 state text NOT NULL,
 entered_at timestamptz NOT NULL,
 started_at timestamptz,
 queue_cut_at timestamptz,
 partial boolean NOT NULL,
 lane_names text[],
 lane_seconds double precision[],
 system_seconds double precision NOT NULL,
 PRIMARY KEY (organization_id,project_id,work_item_id,through_sequence),
 FOREIGN KEY (organization_id,project_id,work_item_id) REFERENCES issues (organization_id,project_id,native_id) ON DELETE CASCADE,
 CHECK ((segment_events=0)=(segment_min_at IS NULL) AND (segment_min_at IS NULL)=(segment_max_at IS NULL) AND segment_min_at<=segment_max_at),
 CHECK ((lane_names IS NULL)=(lane_seconds IS NULL) AND cardinality(lane_names)=cardinality(lane_seconds))
);
CREATE INDEX report_history_checkpoints_cutoff ON report_history_checkpoints (organization_id,project_id,work_item_id,segment_min_at,through_sequence) WHERE segment_events>0;
ALTER TABLE report_history_checkpoints OWNER TO detent_migration;
REVOKE ALL ON report_history_checkpoints FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON report_history_checkpoints TO detent_runtime;
ALTER TABLE report_history_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_history_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON report_history_checkpoints TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose StatementBegin
CREATE FUNCTION report_history_appended() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org text;
 prior_scope text:=current_setting('detent.organization_id',true);
BEGIN
 FOR org IN SELECT DISTINCT organization_id FROM history_rows WHERE type IN ('workflow.transitioned','scheduler.decision') ORDER BY organization_id LOOP
  PERFORM set_config('detent.organization_id',org,true);
  INSERT INTO public.report_issue_checkpoints AS k(organization_id,project_id,work_item_id,revision,history_sequence)
  SELECT org,project_id,work_item_id,1,-1 FROM history_rows
  WHERE organization_id=org AND type IN ('workflow.transitioned','scheduler.decision') GROUP BY project_id,work_item_id
  ON CONFLICT (organization_id,project_id,work_item_id) DO NOTHING;
  UPDATE public.report_issue_checkpoints k
  SET history_sequence=CASE WHEN k.history_sequence=-1 THEN -1 ELSE greatest(k.history_sequence,b.last_seq) END,
      history_through=least(k.history_through,(b.first_seq-1)/256*256)
  FROM (SELECT project_id,work_item_id,min(sequence) AS first_seq,max(sequence) AS last_seq FROM history_rows
        WHERE organization_id=org AND type IN ('workflow.transitioned','scheduler.decision') GROUP BY project_id,work_item_id) b
  WHERE k.organization_id=org AND k.project_id=b.project_id AND k.work_item_id=b.work_item_id;
 END LOOP;
 PERFORM set_config('detent.organization_id',coalesce(prior_scope,''),true);
 RETURN NULL;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION report_history_appended() FROM PUBLIC;
CREATE TRIGGER report_history_appended AFTER INSERT ON collaboration_events
REFERENCING NEW TABLE AS history_rows FOR EACH STATEMENT EXECUTE FUNCTION report_history_appended();

-- +goose StatementBegin
CREATE FUNCTION report_history_reclassified() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org text; event_key text;
 prior_scope text:=current_setting('detent.organization_id',true);
BEGIN
 IF TG_OP='DELETE' THEN
  org:=OLD.organization_id; event_key:=OLD.event_id;
 ELSE
  org:=NEW.organization_id; event_key:=NEW.event_id;
 END IF;
 PERFORM set_config('detent.organization_id',org,true);
 INSERT INTO public.report_issue_checkpoints(organization_id,project_id,work_item_id,revision)
 SELECT org,e.project_id,e.work_item_id,1 FROM public.collaboration_events e WHERE e.organization_id=org AND e.id=event_key AND e.to_state='Rework'
 ON CONFLICT DO NOTHING;
 UPDATE public.report_issue_checkpoints k SET history_through=least(k.history_through,(e.sequence-1)/256*256) FROM public.collaboration_events e
 WHERE e.organization_id=org AND e.id=event_key AND e.to_state='Rework' AND k.organization_id=e.organization_id
 AND k.project_id=e.project_id AND k.work_item_id=e.work_item_id;
 PERFORM set_config('detent.organization_id',coalesce(prior_scope,''),true);
 RETURN NULL;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION report_history_reclassified() FROM PUBLIC;
CREATE TRIGGER report_history_classified AFTER INSERT ON report_event_facts FOR EACH ROW WHEN (NEW.rework_class<>'') EXECUTE FUNCTION report_history_reclassified();
CREATE TRIGGER report_history_reclassified AFTER UPDATE OF rework_class ON report_event_facts FOR EACH ROW WHEN ((OLD.rework_class='')<>(NEW.rework_class='')) EXECUTE FUNCTION report_history_reclassified();
CREATE TRIGGER report_history_unclassified AFTER DELETE ON report_event_facts FOR EACH ROW WHEN (OLD.rework_class<>'') EXECUTE FUNCTION report_history_reclassified();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER report_history_unclassified ON report_event_facts;
DROP TRIGGER report_history_reclassified ON report_event_facts;
DROP TRIGGER report_history_classified ON report_event_facts;
DROP FUNCTION report_history_reclassified();
DROP TRIGGER report_history_appended ON collaboration_events;
DROP FUNCTION report_history_appended();
DROP TABLE report_history_checkpoints;
ALTER TABLE report_checkpoint_cursors DROP COLUMN history_policy_revision;
ALTER TABLE report_issue_checkpoints DROP COLUMN history_through, DROP COLUMN history_sequence, DROP COLUMN history_policy_revision;
