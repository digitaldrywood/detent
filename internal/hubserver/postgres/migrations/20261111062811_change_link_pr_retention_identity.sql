-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE change_issue_links ADD COLUMN retained_pr_version_id text, ADD CONSTRAINT change_issue_links_retained_pr_fk FOREIGN KEY (organization_id,change_id,retained_pr_version_id) REFERENCES change_versions (organization_id,change_id,id) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE change_issue_links DROP CONSTRAINT IF EXISTS change_issue_links_retained_pr_fk, DROP COLUMN IF EXISTS retained_pr_version_id;
