-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE hosted_members ADD COLUMN sessions_revoked_before timestamptz;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'member session revocation history cannot be discarded'; END; $$;
-- +goose StatementEnd
