-- +goose Up
-- +goose StatementBegin
ALTER TABLE conversations ADD COLUMN execution_continuation boolean;
CREATE POLICY continuation_upgrade ON conversations TO detent_migration USING (true) WITH CHECK (true);
CREATE POLICY continuation_upgrade ON conversation_messages FOR SELECT TO detent_migration USING (true);
CREATE POLICY continuation_upgrade ON conversation_commands FOR SELECT TO detent_migration USING (true);
CREATE POLICY continuation_upgrade ON conversation_events FOR SELECT TO detent_migration USING (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS continuation_upgrade ON conversation_events;
DROP POLICY IF EXISTS continuation_upgrade ON conversation_commands;
DROP POLICY IF EXISTS continuation_upgrade ON conversation_messages;
DROP POLICY IF EXISTS continuation_upgrade ON conversations;
ALTER TABLE conversations DROP COLUMN execution_continuation;
-- +goose StatementEnd
