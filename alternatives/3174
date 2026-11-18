-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE change_issue_links VALIDATE CONSTRAINT change_issue_links_retained_pr_fk;

-- +goose Down
SELECT 1;
