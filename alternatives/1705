-- +goose Up
DROP TRIGGER analytics_attempt_revision;

-- +goose Down
-- +goose StatementBegin
CREATE TRIGGER analytics_attempt_revision AFTER UPDATE ON native_attempts
WHEN NEW.analytics_revision=OLD.analytics_revision
BEGIN
 UPDATE native_attempts SET analytics_revision=OLD.analytics_revision+1 WHERE id=NEW.id;
END;
-- +goose StatementEnd
