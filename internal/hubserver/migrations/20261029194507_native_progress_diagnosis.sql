-- +goose Up
-- +goose StatementBegin
ALTER TABLE native_progress_decisions ADD COLUMN diagnosis TEXT NOT NULL DEFAULT '';
ALTER TABLE native_progress_decisions ADD COLUMN instruction TEXT NOT NULL DEFAULT '';
ALTER TABLE native_progress_decisions ADD COLUMN convergence TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE native_progress_decisions DROP COLUMN convergence;
ALTER TABLE native_progress_decisions DROP COLUMN instruction;
ALTER TABLE native_progress_decisions DROP COLUMN diagnosis;
-- +goose StatementEnd
