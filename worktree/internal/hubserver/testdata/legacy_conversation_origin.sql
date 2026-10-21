-- +goose Up
ALTER TABLE conversations ADD COLUMN origin TEXT NOT NULL DEFAULT 'user' CHECK (origin IN ('user', 'worker'));

UPDATE conversations SET origin = 'worker'
WHERE work_item_id IS NOT NULL AND visibility = 'shared'
  AND NOT EXISTS (
    SELECT 1 FROM conversation_audience_events
    WHERE conversation_id = conversations.id AND from_visibility = 'private'
  );

-- +goose Down
ALTER TABLE conversations DROP COLUMN origin;
