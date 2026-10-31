-- +goose Up
CREATE TABLE change_finding_attributions (
  version_id TEXT NOT NULL REFERENCES change_versions(id),
  record_id TEXT NOT NULL,
  role TEXT NOT NULL,
  position INTEGER NOT NULL CHECK (position >= -1),
  attribution TEXT NOT NULL CHECK (attribution IN ('change', 'independent', 'unknown')),
  base_sha TEXT NOT NULL DEFAULT '',
  head_sha TEXT NOT NULL DEFAULT '',
  command TEXT NOT NULL DEFAULT '',
  diagnostic TEXT NOT NULL DEFAULT '',
  head_output_digest TEXT NOT NULL DEFAULT '',
  base_output_digest TEXT NOT NULL DEFAULT '',
  base_exit_code INTEGER NOT NULL DEFAULT -1,
  PRIMARY KEY (version_id, record_id, role, position)
);
-- +goose StatementBegin
CREATE TRIGGER change_finding_attributions_no_update BEFORE UPDATE ON change_finding_attributions
BEGIN
 SELECT RAISE(ABORT, 'finding attribution is immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_finding_attributions_no_delete BEFORE DELETE ON change_finding_attributions
BEGIN
 SELECT RAISE(ABORT, 'finding attribution is immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TABLE change_finding_attributions;
