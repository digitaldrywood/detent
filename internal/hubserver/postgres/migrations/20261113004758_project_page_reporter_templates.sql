-- +goose Up
ALTER TABLE project_page_read_model ADD COLUMN policy_id text NOT NULL DEFAULT '';
ALTER TABLE project_page_read_model ADD COLUMN reporter_bug text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_page_read_model ADD COLUMN reporter_change_request text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE project_page_read_model DROP COLUMN reporter_change_request;
ALTER TABLE project_page_read_model DROP COLUMN reporter_bug;
ALTER TABLE project_page_read_model DROP COLUMN policy_id;
