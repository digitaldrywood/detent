-- +goose NO TRANSACTION
-- +goose Up
DROP POLICY IF EXISTS retained_event_initialization_orgs ON organizations;
CREATE POLICY retained_event_initialization_orgs ON organizations FOR SELECT TO detent_migration USING (true);
-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE initialize_local_board_populations()
LANGUAGE plpgsql AS $$
DECLARE org text; complete boolean; baseline bigint;
BEGIN
 COMMIT;
 FOR org IN SELECT organization_id FROM public.organizations ORDER BY organization_id LOOP
  PERFORM set_config('detent.organization_id',org,true);
  SELECT initialized INTO STRICT complete FROM public.hosted_usage_counters WHERE organization_id=org AND name='retained_collaboration_events';
  COMMIT;
  IF complete THEN CONTINUE; END IF;
  PERFORM set_config('detent.organization_id',org,true);
  SELECT (SELECT count(*) FROM public.collaboration_events WHERE organization_id=org)-(SELECT value FROM public.hosted_usage_counters WHERE organization_id=org AND name='retained_collaboration_events') INTO baseline;
  IF baseline IS NULL THEN RAISE EXCEPTION 'retained event population baseline unavailable'; END IF;
  COMMIT;
  PERFORM set_config('detent.organization_id',org,true);
  PERFORM 1 FROM public.archive_usage_counters WHERE organization_id=org FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'population accounting unavailable'; END IF;
  SELECT initialized INTO STRICT complete FROM public.hosted_usage_counters WHERE organization_id=org AND name='retained_collaboration_events' FOR UPDATE;
  IF NOT complete THEN
   UPDATE public.hosted_usage_counters SET value=value+baseline,initialized=true WHERE organization_id=org AND name='retained_collaboration_events';
   IF EXISTS(SELECT 1 FROM public.hosted_usage_counters WHERE organization_id=org AND name='retained_collaboration_events' AND value<0) THEN RAISE EXCEPTION 'invalid retained event population initialization'; END IF;
  END IF;
  COMMIT;
 END LOOP;
END $$;
-- +goose StatementEnd
REVOKE ALL ON PROCEDURE initialize_local_board_populations() FROM PUBLIC;
CALL initialize_local_board_populations();
DROP PROCEDURE initialize_local_board_populations();
DROP POLICY retained_event_initialization_orgs ON organizations;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'retained event population initialization is forward-only'; END $$;
-- +goose StatementEnd
