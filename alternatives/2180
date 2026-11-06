-- +goose Up
ALTER TABLE native_work_search_lengths ADD COLUMN search_revision INTEGER NOT NULL DEFAULT -1;

DROP TRIGGER native_work_scoped_document_delete;
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_document_delete BEFORE DELETE ON native_work_documents BEGIN
 DELETE FROM native_work_search_postings WHERE document_id=OLD.id;
 DELETE FROM native_work_search_lengths WHERE document_id=OLD.id;
 DELETE FROM native_work_search_refresh WHERE document_id=OLD.id;
 DELETE FROM native_work_scoped_fts WHERE rowid IN(OLD.id*2,OLD.id*2+1);
END;
-- +goose StatementEnd

UPDATE native_work_search_rebuild SET through_id=0,last_id=max(
 COALESCE((SELECT max(id) FROM native_work_documents),0),
 COALESCE((SELECT max(document_id) FROM native_work_search_lengths),0),
 COALESCE((SELECT max(document_id) FROM native_work_search_postings),0)
) WHERE id=1;

-- +goose Down
DROP TRIGGER native_work_scoped_document_delete;
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_document_delete BEFORE DELETE ON native_work_documents BEGIN
 DELETE FROM native_work_scoped_fts WHERE rowid IN(OLD.id*2,OLD.id*2+1);
END;
-- +goose StatementEnd

ALTER TABLE native_work_search_lengths DROP COLUMN search_revision;
