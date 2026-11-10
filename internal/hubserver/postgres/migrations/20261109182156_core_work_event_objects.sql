-- +goose Up
ALTER TABLE core_work_events DROP CONSTRAINT core_work_events_payload_json_check;
ALTER TABLE core_work_events ALTER COLUMN payload_json DROP NOT NULL;
ALTER TABLE core_work_events
 ADD COLUMN payload_object_key text,
 ADD COLUMN payload_sha256 text,
 ADD COLUMN payload_bytes bigint,
 ADD COLUMN payload_content_bytes bigint,
 ADD CONSTRAINT core_work_events_payload_reference CHECK (
  (payload_json IS NOT NULL AND payload_object_key IS NULL AND payload_sha256 IS NULL AND payload_bytes IS NULL AND payload_content_bytes IS NULL)
  OR (payload_json IS NULL AND payload_object_key IS NOT NULL AND payload_sha256 IS NOT NULL AND payload_bytes IS NOT NULL AND payload_content_bytes IS NOT NULL
   AND left(payload_object_key,length('orgs/'||organization_id||'/'))='orgs/'||organization_id||'/'
   AND payload_sha256 ~ '^[0-9a-f]{64}$' AND payload_bytes>0 AND payload_content_bytes>0)
 );
ALTER TABLE core_work_events ADD CONSTRAINT core_work_events_object_writes CHECK (payload_json IS NULL) NOT VALID;
CREATE INDEX core_work_events_payload_idx ON core_work_events (organization_id,payload_object_key) WHERE payload_object_key IS NOT NULL;
CREATE INDEX core_work_events_conversion_idx ON core_work_events (organization_id,source_id) WHERE payload_json IS NOT NULL;
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check CHECK (family IN ('runs','versions','evidence','search','core-work-events'));
ALTER TABLE storage_conversion_progress DROP CONSTRAINT storage_conversion_progress_family_check;
ALTER TABLE storage_conversion_progress ADD CONSTRAINT storage_conversion_progress_family_check CHECK (family IN ('runs','versions','evidence','core-work-events'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION convert_history_payload(
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
    WHEN 'core-work-events' THEN
        UPDATE public.core_work_events r SET
            payload_json = NULL, payload_object_key = target_key, payload_sha256 = target_digest,
            payload_bytes = target_bytes, payload_content_bytes = target_content_bytes
        FROM public.issues i
        WHERE r.organization_id = organization AND r.source_id = record_sequence
          AND r.source_id::text = record_id AND r.issue_id::text = aggregate_id
          AND i.organization_id = organization AND i.id = r.issue_id AND i.project_id = project
          AND r.payload_object_key IS NULL AND r.payload_json = expected_key
          AND octet_length(r.payload_json) = source_content_bytes;
    WHEN 'runs' THEN
        UPDATE public.collaboration_events r SET
            payload_object_key = target_key, payload_sha256 = target_digest,
            payload_bytes = target_bytes, payload_content_bytes = target_content_bytes,
            payload_format_version = 2
        WHERE r.organization_id = organization AND r.project_id = project
          AND r.id = record_id AND r.sequence = record_sequence AND r.work_item_id = aggregate_id
          AND r.type IN ('run.started', 'run.checkpointed', 'run.observed', 'run.finished', 'scheduler.decision')
          AND r.payload_format_version = expected_version AND r.payload_object_key = expected_key
          AND r.payload_sha256 = source_digest AND r.payload_bytes = convert_history_payload.source_bytes
          AND r.payload_content_bytes = convert_history_payload.source_content_bytes;
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
          AND r.payload_sha256 = source_digest AND r.payload_bytes = convert_history_payload.source_bytes
          AND r.payload_content_bytes = convert_history_payload.source_content_bytes;
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
          AND r.payload_sha256 = source_digest AND r.payload_bytes = convert_history_payload.source_bytes
          AND r.payload_content_bytes = convert_history_payload.source_content_bytes;
    ELSE
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'unsupported storage conversion family';
    END CASE;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed = 0 THEN
        RETURN false;
    END IF;
    INSERT INTO public.storage_conversion_progress (organization_id, project_id, family, aggregate_id, converted_rows, converted_bytes)
    VALUES (organization, project, family, aggregate_id, 1, convert_history_payload.source_content_bytes)
    ON CONFLICT ON CONSTRAINT storage_conversion_progress_pkey DO UPDATE SET
        converted_rows = public.storage_conversion_progress.converted_rows + 1,
        converted_bytes = public.storage_conversion_progress.converted_bytes + convert_history_payload.source_content_bytes;
    RETURN true;
END;
$$;
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'core work event conversion is forward-only; preserve verified objects and source backups';
END $$;
-- +goose StatementEnd
