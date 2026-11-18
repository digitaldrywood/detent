-- +goose Up
ALTER TABLE hosted_tenant
 ADD COLUMN notice_seek_due_at timestamptz,
 ADD COLUMN notice_seek_work_item_id text NOT NULL DEFAULT '',
 ADD COLUMN notice_seek_user_id text NOT NULL DEFAULT '',
 ADD COLUMN notice_seek_revision bigint NOT NULL DEFAULT 0 CHECK (notice_seek_revision>=0);

-- +goose Down
ALTER TABLE hosted_tenant
 DROP COLUMN notice_seek_revision,
 DROP COLUMN notice_seek_user_id,
 DROP COLUMN notice_seek_work_item_id,
 DROP COLUMN notice_seek_due_at;
