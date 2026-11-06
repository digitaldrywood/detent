-- +goose Up
CREATE TABLE conversation_tool_revisions (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    text TEXT NOT NULL,
    tool TEXT NOT NULL,
    item_id TEXT NOT NULL,
    status TEXT NOT NULL,
    PRIMARY KEY (conversation_id, message_id, revision)
);

-- +goose Down
DROP TABLE conversation_tool_revisions;
