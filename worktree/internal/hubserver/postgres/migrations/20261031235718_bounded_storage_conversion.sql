-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN payload_format_version bigint NOT NULL DEFAULT 2 CHECK (payload_format_version IN (1, 2));
ALTER TABLE change_versions ADD COLUMN payload_format_version bigint NOT NULL DEFAULT 2 CHECK (payload_format_version IN (1, 2));
ALTER TABLE change_evidence ADD COLUMN payload_format_version bigint NOT NULL DEFAULT 2 CHECK (payload_format_version IN (1, 2));
CREATE INDEX collaboration_events_conversion_idx ON collaboration_events (organization_id, project_id, work_item_id, id, sequence) WHERE payload_format_version = 1;
CREATE INDEX change_versions_conversion_idx ON change_versions (organization_id, change_id, id, number) WHERE payload_format_version = 1;
CREATE INDEX change_evidence_conversion_idx ON change_evidence (organization_id, change_id, sequence) WHERE payload_format_version = 1;

CREATE TABLE storage_conversion_progress (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    family text NOT NULL CHECK (family IN ('runs', 'versions', 'evidence')),
    aggregate_id text NOT NULL,
    converted_rows bigint NOT NULL CHECK (converted_rows > 0),
    converted_bytes bigint NOT NULL CHECK (converted_bytes > 0),
    PRIMARY KEY (organization_id, project_id, family, aggregate_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);
ALTER TABLE storage_conversion_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE storage_conversion_progress FORCE ROW LEVEL SECURITY;
REVOKE ALL ON storage_conversion_progress FROM PUBLIC;
GRANT SELECT ON storage_conversion_progress TO detent_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON storage_conversion_progress TO detent_platform;
CREATE POLICY tenant_isolation ON storage_conversion_progress TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON storage_conversion_progress TO detent_platform USING (true) WITH CHECK (true);

-- +goose StatementBegin
CREATE FUNCTION authorize_history_conversion() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    unchanged record;
BEGIN
    IF TG_OP = 'UPDATE' AND current_user = 'detent_migration'
       AND OLD.payload_format_version = 1 AND NEW.payload_format_version = 2 THEN
        unchanged := NEW;
        unchanged.payload_object_key := OLD.payload_object_key;
        unchanged.payload_sha256 := OLD.payload_sha256;
        unchanged.payload_bytes := OLD.payload_bytes;
        unchanged.payload_content_bytes := OLD.payload_content_bytes;
        unchanged.payload_format_version := OLD.payload_format_version;
        IF unchanged IS NOT DISTINCT FROM OLD THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'history is immutable';
END;
$$;
-- +goose StatementEnd
DROP TRIGGER collaboration_events_immutable ON collaboration_events;
DROP TRIGGER change_versions_immutable ON change_versions;
DROP TRIGGER change_evidence_immutable ON change_evidence;
CREATE TRIGGER collaboration_events_immutable BEFORE UPDATE OR DELETE ON collaboration_events FOR EACH ROW EXECUTE FUNCTION authorize_history_conversion();
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OR DELETE ON change_versions FOR EACH ROW EXECUTE FUNCTION authorize_history_conversion();
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE OR DELETE ON change_evidence FOR EACH ROW EXECUTE FUNCTION authorize_history_conversion();

-- +goose StatementBegin
CREATE FUNCTION convert_history_payload(
    organization text, project text, family text, record_id text, record_sequence bigint,
    aggregate_id text, expected_version bigint, expected_key text, source_digest text,
    source_bytes bigint, source_content_bytes bigint, target_key text, target_digest text,
    target_bytes bigint, target_content_bytes bigint
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    changed bigint;
BEGIN
    IF organization IS DISTINCT FROM NULLIF(current_setting('detent.organization_id', true), '')
       OR expected_version IS DISTINCT FROM 1 THEN
        RETURN false;
    END IF;
    CASE family
    WHEN 'runs' THEN
        UPDATE public.collaboration_events r SET
            payload_object_key = target_key, payload_sha256 = target_digest,
            payload_bytes = target_bytes, payload_content_bytes = target_content_bytes,
            payload_format_version = 2
        WHERE r.organization_id = organization AND r.project_id = project
          AND r.id = record_id AND r.sequence = record_sequence AND r.work_item_id = aggregate_id
          AND r.type IN ('run.started', 'run.checkpointed', 'run.observed', 'run.finished', 'scheduler.decision')
          AND r.payload_format_version = expected_version AND r.payload_object_key = expected_key
          AND r.payload_sha256 = source_digest AND r.payload_bytes = source_bytes
          AND r.payload_content_bytes = source_content_bytes;
    WHEN 'versions' THEN
        UPDATE public.change_versions r SET
            payload_object_key = target_key, payload_sha256 = target_digest,
            payload_bytes = target_bytes, payload_content_bytes = target_content_bytes,
            payload_format_version = 2
        FROM public.change_requests c
        WHERE r.organization_id = organization AND r.id = record_id AND r.number = record_sequence
          AND r.change_id = aggregate_id AND c.organization_id = organization
          AND c.project_id = project AND c.id = r.change_id
          AND r.payload_format_version = expected_version AND r.payload_object_key = expected_key
          AND r.payload_sha256 = source_digest AND r.payload_bytes = source_bytes
          AND r.payload_content_bytes = source_content_bytes;
    WHEN 'evidence' THEN
        UPDATE public.change_evidence r SET
            payload_object_key = target_key, payload_sha256 = target_digest,
            payload_bytes = target_bytes, payload_content_bytes = target_content_bytes,
            payload_format_version = 2
        FROM public.change_requests c
        WHERE r.organization_id = organization AND r.change_id = aggregate_id
          AND record_id = aggregate_id AND r.sequence = record_sequence
          AND c.organization_id = organization AND c.project_id = project AND c.id = r.change_id
          AND r.payload_format_version = expected_version AND r.payload_object_key = expected_key
          AND r.payload_sha256 = source_digest AND r.payload_bytes = source_bytes
          AND r.payload_content_bytes = source_content_bytes;
    ELSE
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'unsupported storage conversion family';
    END CASE;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed = 0 THEN
        RETURN false;
    END IF;
    INSERT INTO public.storage_conversion_progress (organization_id, project_id, family, aggregate_id, converted_rows, converted_bytes)
    VALUES (organization, project, family, aggregate_id, 1, source_content_bytes)
    ON CONFLICT ON CONSTRAINT storage_conversion_progress_pkey DO UPDATE SET
        converted_rows = public.storage_conversion_progress.converted_rows + 1,
        converted_bytes = public.storage_conversion_progress.converted_bytes + source_content_bytes;
    RETURN true;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION convert_history_payload(text, text, text, text, bigint, text, bigint, text, text, bigint, bigint, text, text, bigint, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION convert_history_payload(text, text, text, text, bigint, text, bigint, text, text, bigint, bigint, text, text, bigint, bigint) TO detent_runtime;

-- +goose Down
DROP FUNCTION convert_history_payload(text, text, text, text, bigint, text, bigint, text, text, bigint, bigint, text, text, bigint, bigint);
DROP TRIGGER collaboration_events_immutable ON collaboration_events;
DROP TRIGGER change_versions_immutable ON change_versions;
DROP TRIGGER change_evidence_immutable ON change_evidence;
CREATE TRIGGER collaboration_events_immutable BEFORE UPDATE OR DELETE ON collaboration_events FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OR DELETE ON change_versions FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE OR DELETE ON change_evidence FOR EACH ROW EXECUTE FUNCTION reject_event_change_mutation();
DROP FUNCTION authorize_history_conversion();
DROP TABLE storage_conversion_progress;
ALTER TABLE change_evidence DROP COLUMN payload_format_version;
ALTER TABLE change_versions DROP COLUMN payload_format_version;
ALTER TABLE collaboration_events DROP COLUMN payload_format_version;
