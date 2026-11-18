-- +goose Up
SET LOCAL lock_timeout = '5s';
LOCK TABLE organizations IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE collaboration_events IN SHARE ROW EXCLUSIVE MODE;
DROP POLICY tenant_insert ON hosted_usage_counters;
DROP POLICY tenant_update ON hosted_usage_counters;
DROP POLICY tenant_delete ON hosted_usage_counters;
CREATE POLICY tenant_insert ON hosted_usage_counters FOR INSERT TO detent_runtime WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records','retained_collaboration_events'));
CREATE POLICY tenant_update ON hosted_usage_counters FOR UPDATE TO detent_runtime USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records','retained_collaboration_events')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records','retained_collaboration_events'));
CREATE POLICY tenant_delete ON hosted_usage_counters FOR DELETE TO detent_runtime USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'') AND name NOT IN ('unarchived_issues','history_records','retained_collaboration_events'));
CREATE POLICY retained_event_activation_orgs ON organizations FOR SELECT TO detent_migration USING (true);
CREATE POLICY retained_event_activation_usage ON hosted_usage_counters TO detent_migration USING (name='retained_collaboration_events') WITH CHECK (name='retained_collaboration_events');
INSERT INTO hosted_usage_counters(organization_id,name,value,initialized)
 SELECT organization_id,'retained_collaboration_events',0,false FROM organizations
 ON CONFLICT(organization_id,name) DO UPDATE SET value=0,initialized=false;
DROP POLICY retained_event_activation_usage ON hosted_usage_counters;
DROP POLICY retained_event_activation_orgs ON organizations;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION local_board_history_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org text; previous_scope text:=current_setting('detent.organization_id',true); change bigint; direction integer;
BEGIN
 IF TG_RELID NOT IN ('public.collaboration_versions'::regclass,'public.collaboration_events'::regclass,'public.native_attempt_events'::regclass) OR TG_OP NOT IN ('INSERT','DELETE') THEN RAISE EXCEPTION 'invalid history population source'; END IF;
 IF TG_OP='INSERT' THEN direction:=1; ELSE direction:=-1; END IF;
 FOR org,change IN SELECT organization_id,count(*) FROM local_board_history_rows GROUP BY organization_id ORDER BY organization_id LOOP
  change:=change*direction;
  PERFORM set_config('detent.organization_id',org,true);
  PERFORM 1 FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
  UPDATE public.hosted_usage_counters SET value=value+change WHERE organization_id=org AND name='history_records';
  IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
  IF EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name='history_records' AND initialized AND value<0) THEN RAISE EXCEPTION 'negative history population'; END IF;

  IF TG_RELID='public.collaboration_events'::regclass THEN
   UPDATE public.hosted_usage_counters SET value=value+change WHERE organization_id=org AND name='retained_collaboration_events';
   IF NOT FOUND THEN RAISE EXCEPTION 'retained event population accounting unavailable'; END IF;
   IF EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name='retained_collaboration_events' AND initialized AND value<0) THEN RAISE EXCEPTION 'negative retained event population'; END IF;
  END IF;
 END LOOP;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RETURN NULL;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION local_board_organization_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE previous_scope text:=current_setting('detent.organization_id',true);
BEGIN
 IF TG_RELID<>'public.organizations'::regclass OR TG_OP<>'INSERT' THEN RAISE EXCEPTION 'invalid population organization source'; END IF;
 PERFORM set_config('detent.organization_id',NEW.organization_id,true);
 INSERT INTO public.archive_usage_counters(organization_id,compressed_bytes) VALUES(NEW.organization_id,0) ON CONFLICT DO NOTHING;
 INSERT INTO public.hosted_usage_counters(organization_id,name,value,initialized) VALUES(NEW.organization_id,'unarchived_issues',0,true),(NEW.organization_id,'history_records',0,true),(NEW.organization_id,'retained_collaboration_events',0,true) ON CONFLICT DO NOTHING;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RETURN NEW;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'retained event population ownership is forward-only'; END $$;
-- +goose StatementEnd
