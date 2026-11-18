-- +goose Up
ALTER TABLE organizations ADD COLUMN scheduling_revision bigint NOT NULL DEFAULT 1 CHECK (scheduling_revision >= 0);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'organization scheduling revisions require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
