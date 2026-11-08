-- +goose Up
ALTER TABLE conversations ALTER COLUMN execution_continuation SET NOT NULL;
ALTER TABLE conversations ALTER COLUMN execution_continuation SET DEFAULT false;
DROP POLICY continuation_upgrade ON conversation_events;
DROP POLICY continuation_upgrade ON conversation_commands;
DROP POLICY continuation_upgrade ON conversation_messages;
DROP POLICY continuation_upgrade ON conversations;

-- +goose Down
CREATE POLICY continuation_upgrade ON conversations TO detent_migration USING (true) WITH CHECK (true);
CREATE POLICY continuation_upgrade ON conversation_messages FOR SELECT TO detent_migration USING (true);
CREATE POLICY continuation_upgrade ON conversation_commands FOR SELECT TO detent_migration USING (true);
CREATE POLICY continuation_upgrade ON conversation_events FOR SELECT TO detent_migration USING (true);
ALTER TABLE conversations ALTER COLUMN execution_continuation DROP DEFAULT;
ALTER TABLE conversations ALTER COLUMN execution_continuation DROP NOT NULL;
