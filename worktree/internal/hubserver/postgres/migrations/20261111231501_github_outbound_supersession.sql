-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_outbound ADD COLUMN coalesce_key text NOT NULL DEFAULT '';
ALTER TABLE github_outbound DROP CONSTRAINT github_outbound_status_check;
ALTER TABLE github_outbound ADD CONSTRAINT github_outbound_status_check CHECK (status IN ('pending','processing','retrying','completed','superseded')) NOT VALID;
ALTER TABLE github_outbound ADD CONSTRAINT github_outbound_supersession_check CHECK (status <> 'superseded' OR (outcome IN ('unattempted','rejected') AND lease_id = '')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM github_outbound WHERE status = 'superseded' OR coalesce_key <> '') THEN
  RAISE EXCEPTION 'GitHub outbound supersession custody prevents rollback';
 END IF;
END; $$;
-- +goose StatementEnd
ALTER TABLE github_outbound DROP CONSTRAINT github_outbound_supersession_check;
ALTER TABLE github_outbound DROP CONSTRAINT github_outbound_status_check;
ALTER TABLE github_outbound ADD CONSTRAINT github_outbound_status_check CHECK (status IN ('pending','processing','retrying','completed'));
ALTER TABLE github_outbound DROP COLUMN coalesce_key;
