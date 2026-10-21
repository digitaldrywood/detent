-- +goose Up
CREATE TABLE change_sources (
  version_id TEXT PRIMARY KEY REFERENCES change_versions(id),
  bundle BLOB NOT NULL
);
-- +goose StatementBegin
CREATE TRIGGER change_sources_no_update BEFORE UPDATE ON change_sources
BEGIN
  SELECT RAISE(ABORT, 'change source is immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_sources_no_delete BEFORE DELETE ON change_sources
BEGIN
  SELECT RAISE(ABORT, 'change source is immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TABLE change_sources;
