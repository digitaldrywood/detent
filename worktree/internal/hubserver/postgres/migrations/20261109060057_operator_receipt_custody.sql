-- +goose Up
ALTER TABLE native_command_states
    ADD COLUMN reservation_project_id text,
    ADD COLUMN reservation_correlation_id text,
    ADD COLUMN reservation_claimed_at timestamptz,
    ADD CONSTRAINT native_command_reservation_custody CHECK (
        (reservation_project_id IS NULL AND reservation_correlation_id IS NULL AND reservation_claimed_at IS NULL)
        OR (reservation_project_id IS NOT NULL AND reservation_correlation_id IS NOT NULL
            AND reservation_correlation_id <> '' AND length(reservation_correlation_id) <= 256)
    );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'operator command custody requires an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
