-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE change_versions ADD COLUMN project_id text;
ALTER TABLE change_versions ADD COLUMN landing_history_known boolean NOT NULL DEFAULT false;
ALTER TABLE change_versions ADD COLUMN landed_at timestamptz;
ALTER TABLE change_versions ADD COLUMN landed_head_sha text NOT NULL DEFAULT '';
ALTER TABLE change_versions ADD COLUMN landed_merge_sha text NOT NULL DEFAULT '';
ALTER TABLE change_versions ADD COLUMN landed_base_ref text NOT NULL DEFAULT '';
ALTER TABLE change_versions ADD COLUMN landed_object_key text NOT NULL DEFAULT '';
ALTER TABLE change_versions ADD COLUMN landed_sha256 text NOT NULL DEFAULT '';
ALTER TABLE change_versions ADD COLUMN landed_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE change_versions ADD COLUMN landed_content_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE change_versions ADD COLUMN landed_payload_format_version smallint NOT NULL DEFAULT 0;
ALTER TABLE change_versions ADD CONSTRAINT change_versions_project_identity FOREIGN KEY (organization_id,project_id,change_id) REFERENCES change_requests (organization_id,project_id,id) NOT VALID;
ALTER TABLE change_versions ADD CONSTRAINT change_versions_landing_complete CHECK (
 (landed_at IS NULL AND landed_head_sha='' AND landed_merge_sha='' AND landed_base_ref='' AND landed_object_key='' AND landed_sha256='' AND landed_bytes=0 AND landed_content_bytes=0 AND landed_payload_format_version=0)
 OR (landed_at IS NOT NULL AND landing_history_known AND landed_head_sha=head_sha AND landed_head_sha<>'' AND landed_merge_sha<>'' AND landed_base_ref<>'' AND landed_sha256 ~ '^[0-9a-f]{64}$' AND landed_object_key='orgs/'||organization_id||'/blobs/sha256/'||substr(landed_sha256,1,2)||'/'||landed_sha256 AND landed_bytes>0 AND landed_bytes<=33554432 AND landed_content_bytes>0 AND landed_content_bytes<=33554432 AND landed_payload_format_version IN (1,2))
) NOT VALID;

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
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'version landing provenance cannot be discarded'; END; $$;
-- +goose StatementEnd
