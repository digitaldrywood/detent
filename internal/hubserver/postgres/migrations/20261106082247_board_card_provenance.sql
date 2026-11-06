-- +goose Up
ALTER TABLE native_board_items
 ADD COLUMN card_display_ready boolean NOT NULL DEFAULT false,
 ADD COLUMN card_display_exact boolean NOT NULL DEFAULT false,
 ADD COLUMN card_provenance_provider text,
 ADD COLUMN card_provenance_external_id text,
 ADD COLUMN card_provenance_author_id text,
 ADD COLUMN card_provenance_author_display_name text,
 ADD COLUMN card_provenance_created_at timestamptz,
 ADD COLUMN card_provenance_updated_at timestamptz,
 ADD COLUMN card_provenance_observed_at timestamptz;
CREATE INDEX native_board_display_pending ON native_board_items (organization_id,work_item_id) WHERE NOT card_display_ready;

-- +goose Down
DROP INDEX native_board_display_pending;
ALTER TABLE native_board_items
 DROP COLUMN card_display_ready,
 DROP COLUMN card_display_exact,
 DROP COLUMN card_provenance_provider,
 DROP COLUMN card_provenance_external_id,
 DROP COLUMN card_provenance_author_id,
 DROP COLUMN card_provenance_author_display_name,
 DROP COLUMN card_provenance_created_at,
 DROP COLUMN card_provenance_updated_at,
 DROP COLUMN card_provenance_observed_at;
