-- +goose Up
ALTER TABLE native_board_items
    ADD COLUMN card_change_version_id text,
    ADD COLUMN card_change_policy_id text,
    ADD COLUMN card_change_review_policy_id text,
    ADD COLUMN card_change_base_status text CHECK (card_change_base_status IS NULL OR length(card_change_base_status) BETWEEN 1 AND 64);

-- +goose Down
ALTER TABLE native_board_items
    DROP COLUMN card_change_base_status,
    DROP COLUMN card_change_review_policy_id,
    DROP COLUMN card_change_policy_id,
    DROP COLUMN card_change_version_id;
