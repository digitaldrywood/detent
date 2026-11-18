-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE change_evidence ADD COLUMN quality_at timestamptz;
GRANT UPDATE(quality_at) ON change_evidence TO detent_runtime,detent_platform;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION authorize_history_conversion() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    unchanged record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        unchanged := NEW;
        IF TG_TABLE_NAME = 'change_versions' THEN
            IF NEW.project_id IS NOT NULL AND (OLD.project_id IS NULL OR NEW.project_id = OLD.project_id)
               AND (NOT OLD.landing_history_known OR NEW.landing_history_known)
               AND (OLD.landed_at IS NULL OR (NEW.landed_at,NEW.landed_head_sha,NEW.landed_merge_sha,NEW.landed_base_ref,NEW.landed_object_key,NEW.landed_sha256,NEW.landed_bytes,NEW.landed_content_bytes,NEW.landed_payload_format_version) IS NOT DISTINCT FROM (OLD.landed_at,OLD.landed_head_sha,OLD.landed_merge_sha,OLD.landed_base_ref,OLD.landed_object_key,OLD.landed_sha256,OLD.landed_bytes,OLD.landed_content_bytes,OLD.landed_payload_format_version))
               AND (NEW.project_id,NEW.landing_history_known,NEW.landed_at,NEW.landed_head_sha,NEW.landed_merge_sha,NEW.landed_base_ref,NEW.landed_object_key,NEW.landed_sha256,NEW.landed_bytes,NEW.landed_content_bytes,NEW.landed_payload_format_version) IS DISTINCT FROM (OLD.project_id,OLD.landing_history_known,OLD.landed_at,OLD.landed_head_sha,OLD.landed_merge_sha,OLD.landed_base_ref,OLD.landed_object_key,OLD.landed_sha256,OLD.landed_bytes,OLD.landed_content_bytes,OLD.landed_payload_format_version) THEN
                unchanged.project_id := OLD.project_id;
                unchanged.landing_history_known := OLD.landing_history_known;
                unchanged.landed_at := OLD.landed_at;
                unchanged.landed_head_sha := OLD.landed_head_sha;
                unchanged.landed_merge_sha := OLD.landed_merge_sha;
                unchanged.landed_base_ref := OLD.landed_base_ref;
                unchanged.landed_object_key := OLD.landed_object_key;
                unchanged.landed_sha256 := OLD.landed_sha256;
                unchanged.landed_bytes := OLD.landed_bytes;
                unchanged.landed_content_bytes := OLD.landed_content_bytes;
                unchanged.landed_payload_format_version := OLD.landed_payload_format_version;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
            END IF;
        ELSIF TG_TABLE_NAME = 'change_evidence' THEN
            IF OLD.quality_at IS NULL AND NEW.quality_at IS NOT NULL AND (
                OLD.kind = 'review' AND NEW.quality_at = OLD.created_at OR
                OLD.kind = 'discussion' AND EXISTS(SELECT 1 FROM public.report_escape_facts f WHERE f.organization_id=OLD.organization_id AND f.change_id=OLD.change_id AND f.evidence_sequence=OLD.sequence AND f.observed_at=NEW.quality_at)
            ) THEN
                unchanged.quality_at := OLD.quality_at;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
                unchanged := NEW;
            END IF;
            IF OLD.project_id IS NULL AND NEW.project_id IS NOT NULL THEN
                unchanged.project_id := OLD.project_id;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
                unchanged := NEW;
            END IF;
            IF OLD.kind = 'review' AND (OLD.review_kind = '' OR NOT OLD.review_session_known) AND NEW.review_kind <> '' AND NEW.review_session_known THEN
                unchanged.review_kind := OLD.review_kind;
                unchanged.review_landing := OLD.review_landing;
                unchanged.review_session_known := OLD.review_session_known;
                unchanged.review_session_id := OLD.review_session_id;
                unchanged.review_session_machine := OLD.review_session_machine;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
            END IF;
        ELSIF TG_TABLE_NAME = 'collaboration_events' THEN
            IF OLD.type = 'run.checkpointed' AND NOT OLD.push_known AND NEW.push_known THEN
                unchanged.push_known := OLD.push_known;
                unchanged.push_change_id := OLD.push_change_id;
                unchanged.push_version_id := OLD.push_version_id;
                unchanged.push_head_sha := OLD.push_head_sha;
                unchanged.push_external_effect := OLD.push_external_effect;
                unchanged.push_effect_state := OLD.push_effect_state;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
            END IF;
        END IF;
    END IF;
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

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$ BEGIN IF EXISTS(SELECT 1 FROM change_evidence) THEN RAISE EXCEPTION 'evidence quality time cannot be discarded while history exists'; END IF; END; $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION authorize_history_conversion() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    unchanged record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        unchanged := NEW;
        IF TG_TABLE_NAME = 'change_versions' THEN
            IF NEW.project_id IS NOT NULL AND (OLD.project_id IS NULL OR NEW.project_id = OLD.project_id)
               AND (NOT OLD.landing_history_known OR NEW.landing_history_known)
               AND (OLD.landed_at IS NULL OR (NEW.landed_at,NEW.landed_head_sha,NEW.landed_merge_sha,NEW.landed_base_ref,NEW.landed_object_key,NEW.landed_sha256,NEW.landed_bytes,NEW.landed_content_bytes,NEW.landed_payload_format_version) IS NOT DISTINCT FROM (OLD.landed_at,OLD.landed_head_sha,OLD.landed_merge_sha,OLD.landed_base_ref,OLD.landed_object_key,OLD.landed_sha256,OLD.landed_bytes,OLD.landed_content_bytes,OLD.landed_payload_format_version))
               AND (NEW.project_id,NEW.landing_history_known,NEW.landed_at,NEW.landed_head_sha,NEW.landed_merge_sha,NEW.landed_base_ref,NEW.landed_object_key,NEW.landed_sha256,NEW.landed_bytes,NEW.landed_content_bytes,NEW.landed_payload_format_version) IS DISTINCT FROM (OLD.project_id,OLD.landing_history_known,OLD.landed_at,OLD.landed_head_sha,OLD.landed_merge_sha,OLD.landed_base_ref,OLD.landed_object_key,OLD.landed_sha256,OLD.landed_bytes,OLD.landed_content_bytes,OLD.landed_payload_format_version) THEN
                unchanged.project_id := OLD.project_id;
                unchanged.landing_history_known := OLD.landing_history_known;
                unchanged.landed_at := OLD.landed_at;
                unchanged.landed_head_sha := OLD.landed_head_sha;
                unchanged.landed_merge_sha := OLD.landed_merge_sha;
                unchanged.landed_base_ref := OLD.landed_base_ref;
                unchanged.landed_object_key := OLD.landed_object_key;
                unchanged.landed_sha256 := OLD.landed_sha256;
                unchanged.landed_bytes := OLD.landed_bytes;
                unchanged.landed_content_bytes := OLD.landed_content_bytes;
                unchanged.landed_payload_format_version := OLD.landed_payload_format_version;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
            END IF;
        ELSIF TG_TABLE_NAME = 'change_evidence' THEN
            IF OLD.project_id IS NULL AND NEW.project_id IS NOT NULL THEN
                unchanged.project_id := OLD.project_id;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
                unchanged := NEW;
            END IF;
            IF OLD.kind = 'review' AND (OLD.review_kind = '' OR NOT OLD.review_session_known) AND NEW.review_kind <> '' AND NEW.review_session_known THEN
                unchanged.review_kind := OLD.review_kind;
                unchanged.review_landing := OLD.review_landing;
                unchanged.review_session_known := OLD.review_session_known;
                unchanged.review_session_id := OLD.review_session_id;
                unchanged.review_session_machine := OLD.review_session_machine;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
            END IF;
        ELSIF TG_TABLE_NAME = 'collaboration_events' THEN
            IF OLD.type = 'run.checkpointed' AND NOT OLD.push_known AND NEW.push_known THEN
                unchanged.push_known := OLD.push_known;
                unchanged.push_change_id := OLD.push_change_id;
                unchanged.push_version_id := OLD.push_version_id;
                unchanged.push_head_sha := OLD.push_head_sha;
                unchanged.push_external_effect := OLD.push_external_effect;
                unchanged.push_effect_state := OLD.push_effect_state;
                IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
            END IF;
        END IF;
    END IF;
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
ALTER TABLE change_evidence DROP COLUMN quality_at;
