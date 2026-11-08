-- +goose Up
ALTER TABLE conversations ADD COLUMN message_count bigint NOT NULL DEFAULT 0 CHECK (message_count >= 0);

CREATE POLICY message_count_upgrade ON conversations TO detent_migration USING (true) WITH CHECK (true);
CREATE POLICY message_count_upgrade ON conversation_messages FOR SELECT TO detent_migration USING (true);

UPDATE conversations c SET message_count=m.message_count FROM (
    SELECT organization_id,conversation_id,count(*) AS message_count FROM conversation_messages GROUP BY organization_id,conversation_id
) m WHERE c.organization_id=m.organization_id AND c.id=m.conversation_id;

DROP POLICY message_count_upgrade ON conversation_messages;
DROP POLICY message_count_upgrade ON conversations;

-- +goose StatementBegin
CREATE FUNCTION public.maintain_conversation_message_count() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND OLD.organization_id=NEW.organization_id AND OLD.conversation_id=NEW.conversation_id THEN
        RETURN NULL;
    END IF;
    IF TG_OP IN ('DELETE','UPDATE') THEN
        UPDATE public.conversations SET message_count=message_count-1 WHERE organization_id=OLD.organization_id AND id=OLD.conversation_id;
    END IF;
    IF TG_OP IN ('INSERT','UPDATE') THEN
        UPDATE public.conversations SET message_count=message_count+1,last_message_at=GREATEST(COALESCE(last_message_at,NEW.created_at),NEW.created_at) WHERE organization_id=NEW.organization_id AND id=NEW.conversation_id;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER conversation_message_count AFTER INSERT OR DELETE OR UPDATE OF organization_id,conversation_id ON conversation_messages FOR EACH ROW EXECUTE FUNCTION public.maintain_conversation_message_count();

-- +goose Down
DROP TRIGGER conversation_message_count ON conversation_messages;
DROP FUNCTION public.maintain_conversation_message_count();
ALTER TABLE conversations DROP COLUMN message_count;
