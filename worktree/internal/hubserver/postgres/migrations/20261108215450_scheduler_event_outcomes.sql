-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN scheduler_outcome text NOT NULL DEFAULT '';
CREATE TABLE scheduler_event_heads (
    head_kind text NOT NULL CHECK(head_kind IN ('latest','excluded','unprojected')),
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    id text NOT NULL,
    sequence bigint NOT NULL,
    type text NOT NULL,
    schema_version bigint NOT NULL,
    actor_kind text NOT NULL,
    actor_principal_id text NOT NULL,
    event_revision bigint NOT NULL,
    change_id text NOT NULL,
    version_id text NOT NULL,
    head_sha text NOT NULL,
    run_id text NOT NULL,
    attempt_id text NOT NULL,
    from_state text NOT NULL,
    to_state text NOT NULL,
    recorded_at timestamptz NOT NULL,
    payload_object_key text NOT NULL,
    payload_sha256 text NOT NULL,
    payload_bytes bigint NOT NULL,
    payload_content_bytes bigint NOT NULL,
    payload_format_version bigint NOT NULL,
    landing_waiting boolean NOT NULL,
    landing_change_id text NOT NULL,
    landing_version_id text NOT NULL,
    landing_head_sha text NOT NULL,
    project_sequence bigint NOT NULL,
    push_known boolean NOT NULL,
    push_change_id text NOT NULL,
    push_version_id text NOT NULL,
    push_head_sha text NOT NULL,
    push_external_effect text NOT NULL,
    push_effect_state text NOT NULL,
    scheduler_outcome text NOT NULL,
    PRIMARY KEY(organization_id,project_id,work_item_id,head_kind),
    CHECK(sequence>0 AND type='scheduler.decision'),
    CHECK(head_kind<>'excluded' OR scheduler_outcome='skipped'),
    CHECK(head_kind<>'unprojected' OR scheduler_outcome=''),
    FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES work_item_identities(organization_id,project_id,native_id),
    FOREIGN KEY(organization_id,id) REFERENCES collaboration_events(organization_id,id)
);
CREATE POLICY scheduler_head_backfill ON collaboration_events TO detent_migration USING(true);
INSERT INTO scheduler_event_heads(head_kind, organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, project_sequence, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state, scheduler_outcome)
SELECT k.head_kind, e.organization_id, e.project_id, e.work_item_id, e.id, e.sequence, e.type, e.schema_version, e.actor_kind, e.actor_principal_id, e.event_revision, e.change_id, e.version_id, e.head_sha, e.run_id, e.attempt_id, e.from_state, e.to_state, e.recorded_at, e.payload_object_key, e.payload_sha256, e.payload_bytes, e.payload_content_bytes, e.payload_format_version, e.landing_waiting, e.landing_change_id, e.landing_version_id, e.landing_head_sha, e.project_sequence, e.push_known, e.push_change_id, e.push_version_id, e.push_head_sha, e.push_external_effect, e.push_effect_state, e.scheduler_outcome
FROM (SELECT DISTINCT ON(organization_id,project_id,work_item_id) * FROM collaboration_events WHERE type='scheduler.decision' ORDER BY organization_id,project_id,work_item_id,sequence DESC) e
CROSS JOIN (VALUES ('latest'),('unprojected')) k(head_kind);
DROP POLICY scheduler_head_backfill ON collaboration_events;
ALTER TABLE scheduler_event_heads OWNER TO detent_migration;
REVOKE ALL ON scheduler_event_heads FROM PUBLIC,detent_runtime,detent_platform;
GRANT SELECT ON scheduler_event_heads TO detent_runtime;
ALTER TABLE scheduler_event_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE scheduler_event_heads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON scheduler_event_heads FOR SELECT TO detent_runtime
    USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY migration_access ON scheduler_event_heads TO detent_migration USING(true) WITH CHECK(true);

-- +goose StatementBegin
CREATE FUNCTION capture_scheduler_event_head() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,pg_temp AS $$
DECLARE
    category text;
BEGIN
    IF TG_RELID<>'public.collaboration_events'::regclass OR TG_OP<>'INSERT' OR NEW.type<>'scheduler.decision' THEN
        RAISE EXCEPTION 'invalid scheduler event owner' USING ERRCODE='23514';
    END IF;
    FOREACH category IN ARRAY CASE WHEN NEW.scheduler_outcome='skipped' THEN ARRAY['latest','excluded'] WHEN NEW.scheduler_outcome='' THEN ARRAY['latest','unprojected'] ELSE ARRAY['latest'] END LOOP
        INSERT INTO public.scheduler_event_heads(head_kind, organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, project_sequence, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state, scheduler_outcome)
        VALUES(category, NEW.organization_id, NEW.project_id, NEW.work_item_id, NEW.id, NEW.sequence, NEW.type, NEW.schema_version, NEW.actor_kind, NEW.actor_principal_id, NEW.event_revision, NEW.change_id, NEW.version_id, NEW.head_sha, NEW.run_id, NEW.attempt_id, NEW.from_state, NEW.to_state, NEW.recorded_at, NEW.payload_object_key, NEW.payload_sha256, NEW.payload_bytes, NEW.payload_content_bytes, NEW.payload_format_version, NEW.landing_waiting, NEW.landing_change_id, NEW.landing_version_id, NEW.landing_head_sha, NEW.project_sequence, NEW.push_known, NEW.push_change_id, NEW.push_version_id, NEW.push_head_sha, NEW.push_external_effect, NEW.push_effect_state, NEW.scheduler_outcome)
        ON CONFLICT(organization_id,project_id,work_item_id,head_kind) DO UPDATE SET
        id=EXCLUDED.id,
        sequence=EXCLUDED.sequence,
        type=EXCLUDED.type,
        schema_version=EXCLUDED.schema_version,
        actor_kind=EXCLUDED.actor_kind,
        actor_principal_id=EXCLUDED.actor_principal_id,
        event_revision=EXCLUDED.event_revision,
        change_id=EXCLUDED.change_id,
        version_id=EXCLUDED.version_id,
        head_sha=EXCLUDED.head_sha,
        run_id=EXCLUDED.run_id,
        attempt_id=EXCLUDED.attempt_id,
        from_state=EXCLUDED.from_state,
        to_state=EXCLUDED.to_state,
        recorded_at=EXCLUDED.recorded_at,
        payload_object_key=EXCLUDED.payload_object_key,
        payload_sha256=EXCLUDED.payload_sha256,
        payload_bytes=EXCLUDED.payload_bytes,
        payload_content_bytes=EXCLUDED.payload_content_bytes,
        payload_format_version=EXCLUDED.payload_format_version,
        landing_waiting=EXCLUDED.landing_waiting,
        landing_change_id=EXCLUDED.landing_change_id,
        landing_version_id=EXCLUDED.landing_version_id,
        landing_head_sha=EXCLUDED.landing_head_sha,
        project_sequence=EXCLUDED.project_sequence,
        push_known=EXCLUDED.push_known,
        push_change_id=EXCLUDED.push_change_id,
        push_version_id=EXCLUDED.push_version_id,
        push_head_sha=EXCLUDED.push_head_sha,
        push_external_effect=EXCLUDED.push_external_effect,
        push_effect_state=EXCLUDED.push_effect_state,
        scheduler_outcome=EXCLUDED.scheduler_outcome
        WHERE EXCLUDED.sequence>scheduler_event_heads.sequence;
    END LOOP;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION capture_scheduler_event_head() OWNER TO detent_migration;
REVOKE ALL ON FUNCTION capture_scheduler_event_head() FROM PUBLIC,detent_runtime,detent_platform;
CREATE TRIGGER collaboration_events_scheduler_head AFTER INSERT ON collaboration_events
    FOR EACH ROW WHEN (NEW.type='scheduler.decision') EXECUTE FUNCTION capture_scheduler_event_head();

-- +goose Down
DROP TRIGGER collaboration_events_scheduler_head ON collaboration_events;
DROP FUNCTION capture_scheduler_event_head();
DROP TABLE scheduler_event_heads;
ALTER TABLE collaboration_events DROP COLUMN scheduler_outcome;
