-- +goose Up
DROP TRIGGER change_sources_no_update;
-- +goose StatementBegin
CREATE TRIGGER change_sources_no_update BEFORE UPDATE ON change_sources
WHEN NEW.version_id IS NOT OLD.version_id
  OR substr(OLD.bundle,1,4) = x'00444353'
  OR NOT (
    (substr(NEW.bundle,1,6) = x'004443530101' AND length(NEW.bundle) < length(OLD.bundle))
    OR (substr(NEW.bundle,1,6) = x'004443530100' AND substr(NEW.bundle,7) = OLD.bundle)
  )
BEGIN
  SELECT RAISE(ABORT, 'change source is immutable');
END;
-- +goose StatementEnd
CREATE INDEX change_sources_legacy_encoding_idx ON change_sources(version_id)
WHERE substr(bundle,1,4) <> x'00444353';

-- +goose Down
DROP INDEX change_sources_legacy_encoding_idx;
DROP TRIGGER change_sources_no_update;
-- +goose StatementBegin
CREATE TRIGGER change_sources_no_update BEFORE UPDATE ON change_sources
BEGIN
  SELECT RAISE(ABORT, 'change source is immutable');
END;
-- +goose StatementEnd
