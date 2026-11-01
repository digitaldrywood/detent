-- +goose Up
CREATE TABLE linked_issue_sources (
  work_item_id TEXT PRIMARY KEY REFERENCES issues(native_id),
  source_url TEXT NOT NULL,
  title_supplied INTEGER NOT NULL CHECK (title_supplied IN (0, 1)),
  body_supplied INTEGER NOT NULL CHECK (body_supplied IN (0, 1)),
  snapshot_json TEXT CHECK (snapshot_json IS NULL OR json_valid(snapshot_json))
);

-- +goose Down
DROP TABLE linked_issue_sources;
