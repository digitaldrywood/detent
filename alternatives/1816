-- +goose Up
DROP INDEX native_board_expiry_idx;
UPDATE native_board_items SET expires_at='' WHERE running=0 AND json_extract(record_json,'$.execution') IS NULL;
CREATE INDEX native_board_expiry_idx ON native_board_items(expires_at,work_item_id) WHERE expires_at<>'';

-- +goose Down
DROP INDEX native_board_expiry_idx;
CREATE INDEX native_board_expiry_idx ON native_board_items(expires_at) WHERE running=1;
