-- +goose Up
ALTER TABLE native_board_deltas
 ADD COLUMN topic text NOT NULL DEFAULT 'card',
 ADD COLUMN entity_id text NOT NULL DEFAULT '',
 ADD COLUMN entity_revision bigint,
 ADD COLUMN kind text NOT NULL DEFAULT 'upsert',
 ADD COLUMN status text NOT NULL DEFAULT '',
 ADD COLUMN related_id text NOT NULL DEFAULT '';

ALTER TABLE native_board_deltas NO FORCE ROW LEVEL SECURITY;
UPDATE native_board_deltas SET entity_id=work_item_id,
 kind=CASE WHEN current_object_key IS NULL THEN 'delete' ELSE 'upsert' END;
ALTER TABLE native_board_deltas FORCE ROW LEVEL SECURITY;

ALTER TABLE native_board_deltas DROP CONSTRAINT native_board_deltas_check2;
ALTER TABLE native_board_deltas ADD CONSTRAINT native_board_delta_topic_payload CHECK (
 (topic='card' AND work_item_id<>'' AND entity_id=work_item_id
  AND (previous_object_key IS NOT NULL OR current_object_key IS NOT NULL))
 OR (topic IN ('work_item','board','lane','activity','human_action','health','workspace','action_run')
  AND entity_id<>'' AND previous_object_key IS NULL AND current_object_key IS NULL)
);
ALTER TABLE native_board_deltas ADD CONSTRAINT native_board_delta_entity_revision CHECK (entity_revision IS NULL OR entity_revision>=0);
ALTER TABLE native_board_deltas ADD CONSTRAINT native_board_delta_kind CHECK (kind IN ('upsert','delete','invalidate'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'Project delivery notice history cannot be removed by rollback';
END $$;
-- +goose StatementEnd
