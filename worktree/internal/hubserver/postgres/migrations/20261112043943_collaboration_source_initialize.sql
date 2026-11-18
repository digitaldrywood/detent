-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout='5s';
ALTER TABLE collaboration_versions VALIDATE CONSTRAINT collaboration_source_verified;
ALTER TABLE issues VALIDATE CONSTRAINT issues_scheduled_source_facts;
ALTER TABLE native_comments VALIDATE CONSTRAINT native_comments_scheduled_source_facts;
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'scheduled source provenance cannot be discarded'; END; $$;
-- +goose StatementEnd
