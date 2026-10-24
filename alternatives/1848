-- +goose Up
CREATE TABLE quality_landings (
  version_id TEXT PRIMARY KEY REFERENCES change_versions(id),
  record_json TEXT NOT NULL CHECK (json_valid(record_json))
);

-- +goose Down
DROP TABLE quality_landings;
