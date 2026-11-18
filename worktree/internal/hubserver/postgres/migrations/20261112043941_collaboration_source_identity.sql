-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout='5s';
ALTER TABLE collaboration_versions ADD COLUMN IF NOT EXISTS source_known boolean NOT NULL DEFAULT false, ADD COLUMN IF NOT EXISTS source_scheduled boolean NOT NULL DEFAULT false, ADD COLUMN IF NOT EXISTS source_observed_at timestamptz NOT NULL DEFAULT 'epoch';
ALTER TABLE issues ADD COLUMN IF NOT EXISTS source_revision bigint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS source_scheduled boolean NOT NULL DEFAULT false, ADD COLUMN IF NOT EXISTS source_observed_at timestamptz;
ALTER TABLE native_comments ADD COLUMN IF NOT EXISTS source_revision bigint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS source_scheduled boolean NOT NULL DEFAULT false, ADD COLUMN IF NOT EXISTS source_observed_at timestamptz;
ALTER TABLE collaboration_versions DROP CONSTRAINT IF EXISTS collaboration_source_verified;
ALTER TABLE collaboration_versions ADD CONSTRAINT collaboration_source_verified CHECK(NOT source_scheduled OR source_known) NOT VALID;
ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_scheduled_source_facts;
ALTER TABLE issues ADD CONSTRAINT issues_scheduled_source_facts CHECK(source_revision>=0 AND source_revision<=revision AND ((source_revision=0 AND NOT source_scheduled AND source_observed_at IS NULL) OR (source_revision>0 AND source_observed_at IS NOT NULL))) NOT VALID;
ALTER TABLE native_comments DROP CONSTRAINT IF EXISTS native_comments_scheduled_source_facts;
ALTER TABLE native_comments ADD CONSTRAINT native_comments_scheduled_source_facts CHECK(source_revision>=0 AND source_revision<=revision AND ((source_revision=0 AND NOT source_scheduled AND source_observed_at IS NULL) OR (source_revision>0 AND source_observed_at IS NOT NULL))) NOT VALID;
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'scheduled source provenance cannot be discarded'; END; $$;
-- +goose StatementEnd
