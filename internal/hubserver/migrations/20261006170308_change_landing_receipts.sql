-- +goose Up
CREATE TABLE change_landing_receipts (
  attempt_id TEXT PRIMARY KEY REFERENCES native_attempts(id),
  version_id TEXT NOT NULL REFERENCES change_versions(id),
  record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE INDEX change_landing_receipts_version_idx ON change_landing_receipts(version_id);

-- +goose Down
DROP TABLE change_landing_receipts;
