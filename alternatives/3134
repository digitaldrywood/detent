-- +goose Up
SET LOCAL lock_timeout = '5s';
DROP TRIGGER local_board_version_population ON collaboration_versions;
DROP TRIGGER local_board_event_population ON collaboration_events;
DROP TRIGGER local_board_attempt_population ON native_attempt_events;

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
 END LOOP;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RETURN NULL;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
CREATE TRIGGER local_board_version_population AFTER INSERT ON collaboration_versions
REFERENCING NEW TABLE AS local_board_history_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_version_population_delete AFTER DELETE ON collaboration_versions
REFERENCING OLD TABLE AS local_board_history_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_event_population AFTER INSERT ON collaboration_events
REFERENCING NEW TABLE AS local_board_history_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_event_population_delete AFTER DELETE ON collaboration_events
REFERENCING OLD TABLE AS local_board_history_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_attempt_population AFTER INSERT ON native_attempt_events
REFERENCING NEW TABLE AS local_board_history_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_attempt_population_delete AFTER DELETE ON native_attempt_events
REFERENCING OLD TABLE AS local_board_history_rows FOR EACH STATEMENT EXECUTE FUNCTION local_board_history_population();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER local_board_version_population_delete ON collaboration_versions;
DROP TRIGGER local_board_version_population ON collaboration_versions;
DROP TRIGGER local_board_event_population_delete ON collaboration_events;
DROP TRIGGER local_board_event_population ON collaboration_events;
DROP TRIGGER local_board_attempt_population_delete ON native_attempt_events;
DROP TRIGGER local_board_attempt_population ON native_attempt_events;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION local_board_history_population() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org text; previous_scope text:=current_setting('detent.organization_id',true); change bigint;
BEGIN
 IF TG_RELID NOT IN ('public.collaboration_versions'::regclass,'public.collaboration_events'::regclass,'public.native_attempt_events'::regclass) OR TG_OP NOT IN ('INSERT','DELETE') THEN RAISE EXCEPTION 'invalid history population source'; END IF;
 IF TG_OP='INSERT' THEN org:=NEW.organization_id; change:=1; ELSE org:=OLD.organization_id; change:=-1; END IF;
 PERFORM set_config('detent.organization_id',org,true);
 PERFORM 1 FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 UPDATE public.hosted_usage_counters SET value=value+change WHERE organization_id=org AND name='history_records';
 IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
 IF EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name='history_records' AND initialized AND value<0) THEN RAISE EXCEPTION 'negative history population'; END IF;
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
EXCEPTION WHEN OTHERS THEN
 PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
 RAISE;
END $$;
-- +goose StatementEnd
CREATE TRIGGER local_board_version_population AFTER INSERT OR DELETE ON collaboration_versions FOR EACH ROW EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_event_population AFTER INSERT OR DELETE ON collaboration_events FOR EACH ROW EXECUTE FUNCTION local_board_history_population();
CREATE TRIGGER local_board_attempt_population AFTER INSERT OR DELETE ON native_attempt_events FOR EACH ROW EXECUTE FUNCTION local_board_history_population();
