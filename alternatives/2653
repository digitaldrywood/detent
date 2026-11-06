-- +goose Up
ALTER TABLE issues ADD COLUMN dispatch_generation bigint NOT NULL DEFAULT 0 CHECK (dispatch_generation >= 0);
ALTER TABLE issues ADD COLUMN dispatch_requested_at timestamptz;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'dispatch requests require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
