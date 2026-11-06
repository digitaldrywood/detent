-- +goose Up
DELETE FROM team_thread_unread_summaries;
ALTER TABLE team_thread_unread_summaries ADD COLUMN scope_hash TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX team_thread_unread_member ON team_thread_unread_summaries(organization_id,principal_id);

-- +goose Down
DROP INDEX team_thread_unread_member;
ALTER TABLE team_thread_unread_summaries DROP COLUMN scope_hash;
