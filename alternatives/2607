-- +goose Up
ALTER TABLE cost_observation_revisions ALTER COLUMN received_at TYPE text
USING to_char(received_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '000Z';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM cost_observation_revisions WHERE right(received_at, 4) <> '000Z') THEN
        RAISE EXCEPTION 'cost receipt timestamps require nanosecond precision';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE cost_observation_revisions ALTER COLUMN received_at TYPE timestamptz
USING received_at::timestamptz;
