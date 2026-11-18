-- +goose Up
ALTER TABLE change_evidence ADD COLUMN review_kind text NOT NULL DEFAULT '' CHECK (review_kind IN ('','human','validator','implementation','security_audit')), ADD COLUMN review_landing boolean NOT NULL DEFAULT false,
 ADD COLUMN review_session_known boolean NOT NULL DEFAULT false,
 ADD COLUMN review_session_id bigint NOT NULL DEFAULT 0 CHECK (review_session_id>=0),
 ADD COLUMN review_session_machine text NOT NULL DEFAULT '';
ALTER TABLE collaboration_events
 ADD COLUMN push_known boolean NOT NULL DEFAULT false,
 ADD COLUMN push_change_id text NOT NULL DEFAULT '',
 ADD COLUMN push_version_id text NOT NULL DEFAULT '',
 ADD COLUMN push_head_sha text NOT NULL DEFAULT '',
 ADD COLUMN push_external_effect text NOT NULL DEFAULT '',
 ADD COLUMN push_effect_state text NOT NULL DEFAULT '';
CREATE INDEX change_unprojected_reviews_idx ON change_evidence (organization_id,change_id,sequence) WHERE kind='review' AND (review_kind='' OR NOT review_session_known);
CREATE INDEX change_validator_unprojected_idx ON change_evidence (organization_id,change_id,version_id,sequence) WHERE kind='review' AND (review_kind='' OR NOT review_session_known);
CREATE INDEX change_validator_session_idx ON change_evidence (organization_id,change_id,version_id,review_session_machine,review_session_id,review_kind,sequence DESC) WHERE kind='review' AND review_session_known;
CREATE INDEX change_validator_implementation_idx ON change_evidence (organization_id,change_id,version_id,sequence DESC) WHERE kind='review' AND review_kind='implementation' AND NOT review_landing;
CREATE INDEX change_summary_reviews_idx ON change_evidence (organization_id,change_id,version_id,review_kind,actor_principal_id,sequence DESC) WHERE kind='review';
CREATE INDEX change_summary_checks_idx ON change_evidence (organization_id,change_id,version_id,sequence) WHERE kind='check';
CREATE INDEX change_publication_checkpoint_idx ON collaboration_events (organization_id,project_id,work_item_id,push_change_id,push_version_id,push_head_sha,sequence DESC) WHERE type='run.checkpointed' AND push_external_effect='git_push' AND push_effect_state='confirmed';
CREATE INDEX change_publication_unprojected_idx ON collaboration_events (organization_id,project_id,work_item_id,sequence DESC) WHERE type='run.checkpointed' AND NOT push_known;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION authorize_history_conversion() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    unchanged record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        unchanged := NEW;
        IF TG_TABLE_NAME = 'change_evidence' THEN
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
CREATE OR REPLACE FUNCTION authorize_history_conversion() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
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
DROP INDEX change_unprojected_reviews_idx;
DROP INDEX change_validator_unprojected_idx;
DROP INDEX change_validator_session_idx;
DROP INDEX change_validator_implementation_idx;
DROP INDEX change_publication_unprojected_idx;
DROP INDEX change_publication_checkpoint_idx;
DROP INDEX change_summary_checks_idx;
DROP INDEX change_summary_reviews_idx;
ALTER TABLE collaboration_events DROP COLUMN push_known,DROP COLUMN push_change_id,DROP COLUMN push_version_id,DROP COLUMN push_head_sha,DROP COLUMN push_external_effect,DROP COLUMN push_effect_state;
ALTER TABLE change_evidence DROP COLUMN review_kind,DROP COLUMN review_landing,DROP COLUMN review_session_known,DROP COLUMN review_session_id,DROP COLUMN review_session_machine;
