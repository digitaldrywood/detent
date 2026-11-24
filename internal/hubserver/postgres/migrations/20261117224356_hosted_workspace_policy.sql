-- +goose Up
ALTER TABLE platform.organizations ADD COLUMN workspace_policy_key text CHECK (workspace_policy_key = '' OR workspace_policy_key ~ '^platform/entry/sha256/[0-9a-f]{64}$');

ALTER TABLE platform.organizations ALTER COLUMN workspace_policy_key SET DEFAULT '';

CREATE INDEX organizations_active_provisions ON platform.organizations (id) WHERE managed AND state='allocating';

CREATE INDEX organizations_due_provisions ON platform.organizations (COALESCE(next_attempt_at,'-infinity'::timestamptz),created_at,id) WHERE managed AND state IN ('requested','allocating');
CREATE INDEX organizations_pending_deletion ON platform.organizations (id) WHERE managed AND state='deleting';

-- +goose Down
DROP INDEX platform.organizations_pending_deletion;
DROP INDEX platform.organizations_due_provisions;
DROP INDEX platform.organizations_active_provisions;
ALTER TABLE platform.organizations DROP COLUMN workspace_policy_key;
