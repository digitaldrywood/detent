-- +goose Up
-- +goose StatementBegin
ALTER TABLE native_attempts
    ADD COLUMN started_at_submicro_ns smallint NOT NULL DEFAULT 0
    CHECK (started_at_submicro_ns BETWEEN 0 AND 999);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM native_attempts WHERE started_at_submicro_ns <> 0) THEN
        RAISE EXCEPTION 'attempt start timestamps require sub-microsecond precision';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE native_attempts DROP COLUMN started_at_submicro_ns;
