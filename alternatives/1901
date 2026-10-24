-- +goose Up
UPDATE event_compactions SET scanned_sequence=0 WHERE aggregate_kind='work_item' AND scanned_sequence<>0;

-- +goose Down
SELECT 1;
