-- +goose Up
PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql=replace(sql,' REFERENCES native_work_documents(id) ON DELETE CASCADE','')
WHERE type='table' AND name IN('native_work_search_lengths','native_work_search_refresh');
UPDATE sqlite_schema SET sql=replace(sql,',
 FOREIGN KEY(document_id,scope) REFERENCES native_work_search_lengths(document_id,scope) ON DELETE CASCADE','')
WHERE type='table' AND name='native_work_search_postings';
PRAGMA writable_schema = RESET;

DROP TRIGGER native_work_search_document_insert;
-- +goose StatementBegin
CREATE TRIGGER native_work_search_document_insert AFTER INSERT ON native_work_documents BEGIN
 INSERT INTO native_work_search_refresh SELECT NEW.id,organization_id,project_id FROM issues WHERE id=NEW.issue_id
 ON CONFLICT(document_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id;
END;
-- +goose StatementEnd

DROP TRIGGER native_work_scoped_document_delete;
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_document_delete BEFORE DELETE ON native_work_documents BEGIN
 INSERT OR IGNORE INTO native_work_search_refresh(document_id) VALUES(OLD.id);
 DELETE FROM native_work_scoped_fts WHERE rowid IN(OLD.id*2,OLD.id*2+1);
END;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
