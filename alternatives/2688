-- +goose Up
ALTER TABLE daily_summary_preferences
 ALTER COLUMN window_end TYPE timestamptz USING to_timestamp(window_end),
 ALTER COLUMN due_at TYPE timestamptz USING to_timestamp(due_at);
ALTER TABLE daily_summary_receipts
 ALTER COLUMN window_end TYPE timestamptz USING to_timestamp(window_end),
 ALTER COLUMN recorded_at TYPE timestamptz USING to_timestamp(recorded_at);
-- +goose Down
ALTER TABLE daily_summary_receipts
 ALTER COLUMN window_end TYPE bigint USING floor(extract(epoch FROM window_end))::bigint,
 ALTER COLUMN recorded_at TYPE bigint USING floor(extract(epoch FROM recorded_at))::bigint;
ALTER TABLE daily_summary_preferences
 ALTER COLUMN window_end TYPE bigint USING floor(extract(epoch FROM window_end))::bigint,
 ALTER COLUMN due_at TYPE bigint USING floor(extract(epoch FROM due_at))::bigint;
