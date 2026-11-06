-- +goose Up
CREATE INDEX queue_entries_issue_scope_idx ON queue_entries(issue_id, scope, id);
CREATE INDEX issue_dependencies_blocker_idx ON issue_dependencies(blocker_issue_id, dependent_issue_id);

-- +goose Down
DROP INDEX issue_dependencies_blocker_idx;
DROP INDEX queue_entries_issue_scope_idx;
