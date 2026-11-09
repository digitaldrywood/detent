-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE conversation_attachments
 ADD COLUMN publication_pending boolean NOT NULL DEFAULT false,
 ADD COLUMN publication_operation text NOT NULL DEFAULT '',
 ADD COLUMN publication_key text NOT NULL DEFAULT '',
 ADD COLUMN publication_hash text NOT NULL DEFAULT '',
 ADD CONSTRAINT conversation_attachment_pending_publication CHECK (
  NOT publication_pending OR (message_id IS NULL AND publication_operation<>'' AND publication_key<>'' AND publication_hash<>'')
 ) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE conversation_attachments
 DROP CONSTRAINT conversation_attachment_pending_publication,
 DROP COLUMN publication_hash,
 DROP COLUMN publication_key,
 DROP COLUMN publication_operation,
 DROP COLUMN publication_pending;
