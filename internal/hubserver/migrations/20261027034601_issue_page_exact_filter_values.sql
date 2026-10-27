-- +goose Up
ALTER TABLE issues ADD COLUMN page_filters_ready INTEGER;
ALTER TABLE native_board_items ADD COLUMN page_filters_ready INTEGER;
CREATE INDEX issues_page_filters_unknown ON issues(organization_id,project_id,id) WHERE page_filters_ready IS NULL;
CREATE INDEX native_board_page_filters_unknown ON native_board_items(organization_id,project_id,work_item_id) WHERE page_filters_ready IS NULL;
CREATE TABLE native_issue_page_filter_values (
 issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('label','assignee')),
 value TEXT NOT NULL,
 PRIMARY KEY(issue_id,kind,value)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE native_issue_page_filter_values;
DROP INDEX native_board_page_filters_unknown;
DROP INDEX issues_page_filters_unknown;
ALTER TABLE native_board_items DROP COLUMN page_filters_ready;
ALTER TABLE issues DROP COLUMN page_filters_ready;
