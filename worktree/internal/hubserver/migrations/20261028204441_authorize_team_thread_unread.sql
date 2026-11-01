-- +goose Up
ALTER TABLE team_thread_unread_summaries ADD COLUMN reporter_principal TEXT NOT NULL DEFAULT '';
UPDATE team_thread_unread_summaries SET scope_hash='',unread_count=NULL,requested_revision=requested_revision+1,scan_revision=0,after_thread='',through_thread='',partial_count=0;

-- +goose Down
ALTER TABLE team_thread_unread_summaries DROP COLUMN reporter_principal;
