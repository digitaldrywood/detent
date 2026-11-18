-- +goose Up
-- +goose StatementBegin
ALTER TABLE native_attempts ADD COLUMN cleanup_acknowledged_at timestamptz;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE native_attempts DROP COLUMN cleanup_acknowledged_at;
-- +goose StatementEnd
