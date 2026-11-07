-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE health_findings
ADD COLUMN page_event_ids text[],
ADD COLUMN page_attempt_ids text[],
ADD COLUMN page_work_item_ids text[],
ADD COLUMN page_signatures text[],
ADD COLUMN page_reason text NOT NULL DEFAULT '',
ADD COLUMN page_count_keys text[],
ADD COLUMN page_count_values bigint[],
ADD COLUMN page_value_keys text[],
ADD COLUMN page_value_values double precision[],
ADD COLUMN page_queue_keys text[],
ADD COLUMN page_queue_depths bigint[],
ADD COLUMN page_queue_short double precision[],
ADD COLUMN page_queue_long double precision[],
ADD COLUMN page_queue_baseline double precision[],
ADD COLUMN page_productivity_from timestamptz,
ADD COLUMN page_productivity_to timestamptz,
ADD COLUMN page_failure_stage text NOT NULL DEFAULT '',
ADD COLUMN page_last_report_at timestamptz,
ADD COLUMN page_last_run_url text NOT NULL DEFAULT '',
ADD COLUMN page_last_green_at timestamptz,
ADD COLUMN page_cadence_seconds bigint NOT NULL DEFAULT 0,
ADD COLUMN page_window_seconds bigint NOT NULL DEFAULT 0,
ADD COLUMN page_generation bigint NOT NULL DEFAULT 1,
ADD COLUMN page_checkpoint bigint NOT NULL DEFAULT 0,
ADD COLUMN page_source_at timestamptz NOT NULL DEFAULT clock_timestamp(),
ADD COLUMN page_projected_at timestamptz,
ADD COLUMN page_examined_at timestamptz NOT NULL DEFAULT 'epoch';

-- +goose StatementBegin
CREATE FUNCTION invalidate_health_page() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.evidence_object_key,NEW.evidence_sha256,NEW.evidence_bytes,NEW.evidence_content_bytes) IS DISTINCT FROM (OLD.evidence_object_key,OLD.evidence_sha256,OLD.evidence_bytes,OLD.evidence_content_bytes) THEN
 NEW.page_generation=OLD.page_generation+1;
 NEW.page_source_at=clock_timestamp();
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER health_page_source BEFORE UPDATE ON health_findings FOR EACH ROW EXECUTE FUNCTION invalidate_health_page();
GRANT UPDATE ON health_findings TO detent_runtime;
CREATE INDEX CONCURRENTLY health_page_pending ON health_findings(organization_id,page_examined_at,id) WHERE page_checkpoint<page_generation;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'health page evidence requires explicit retained-data rollback';
END $$;
-- +goose StatementEnd
