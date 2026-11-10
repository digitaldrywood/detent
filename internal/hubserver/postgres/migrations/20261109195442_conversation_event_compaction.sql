-- +goose Up
ALTER TABLE conversations
    ADD COLUMN compaction_through_sequence bigint NOT NULL DEFAULT 0 CHECK (compaction_through_sequence >= 0 AND compaction_through_sequence <= event_seq),
    ADD COLUMN compaction_scanned_sequence bigint NOT NULL DEFAULT 0 CHECK (compaction_scanned_sequence >= 0 AND compaction_scanned_sequence <= event_seq),
    ADD COLUMN compaction_content_scanned_sequence bigint NOT NULL DEFAULT 0 CHECK (compaction_content_scanned_sequence >= 0 AND compaction_content_scanned_sequence <= event_seq),
    ADD COLUMN compaction_content_id text NOT NULL DEFAULT '',
    ADD CONSTRAINT conversations_compaction_reference_check CHECK ((compaction_through_sequence=0)=(compaction_content_id=''));

ALTER TABLE conversation_events ADD COLUMN compaction_group_key text;

CREATE INDEX conversations_compaction_candidates_idx
    ON conversations (organization_id, (CASE WHEN compaction_content_scanned_sequence<event_seq THEN '-infinity'::timestamptz ELSE GREATEST(settled_at,updated_at,COALESCE(last_message_at,updated_at)) END),id)
    WHERE deleted_at IS NULL AND (compaction_content_scanned_sequence<event_seq OR (status='settled' AND settled_at IS NOT NULL AND work_item_id IS NULL AND subject_work_item_id IS NULL
      AND execution_status IN ('idle','completed','interrupted','failed') AND compaction_scanned_sequence<event_seq));
CREATE INDEX conversation_events_unprojected_idx
    ON conversation_events (organization_id,conversation_id,seq) WHERE query_metadata_version=0 OR compaction_group_key IS NULL;
CREATE INDEX conversation_events_compaction_group_idx
    ON conversation_events (organization_id,conversation_id,type,compaction_group_key,seq);
CREATE INDEX conversation_events_compaction_neighbor_idx
    ON conversation_events (organization_id,conversation_id,type,seq);

-- +goose Down
DROP INDEX conversation_events_compaction_neighbor_idx;
DROP INDEX conversation_events_compaction_group_idx;
DROP INDEX conversation_events_unprojected_idx;
DROP INDEX conversations_compaction_candidates_idx;
ALTER TABLE conversation_events DROP COLUMN compaction_group_key;
ALTER TABLE conversations DROP CONSTRAINT conversations_compaction_reference_check, DROP COLUMN compaction_content_scanned_sequence, DROP COLUMN compaction_content_id, DROP COLUMN compaction_scanned_sequence, DROP COLUMN compaction_through_sequence;
