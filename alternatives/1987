-- +goose Up
CREATE TABLE native_issue_pages (
  id TEXT PRIMARY KEY,
  scope TEXT NOT NULL,
  expires INTEGER NOT NULL
);
CREATE INDEX native_issue_pages_expiry ON native_issue_pages(expires);

CREATE TABLE native_issue_page_items (
  page_id TEXT NOT NULL REFERENCES native_issue_pages(id) ON DELETE CASCADE,
  native_id TEXT NOT NULL,
  lane_rank INTEGER NOT NULL,
  state_name TEXT NOT NULL,
  priority_rank INTEGER NOT NULL,
  activity_missing INTEGER NOT NULL,
  activity_at TEXT NOT NULL,
  identifier TEXT NOT NULL,
  PRIMARY KEY (page_id, native_id)
);
CREATE INDEX native_issue_page_order ON native_issue_page_items (
  page_id, lane_rank, state_name, priority_rank, activity_missing, activity_at DESC, identifier
);

-- +goose Down
DROP TABLE native_issue_page_items;
DROP TABLE native_issue_pages;
