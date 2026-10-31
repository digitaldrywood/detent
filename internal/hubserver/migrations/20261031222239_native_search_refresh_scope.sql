-- +goose Up
DROP TRIGGER native_work_search_document_insert;
-- +goose StatementBegin
CREATE TRIGGER native_work_search_document_insert AFTER INSERT ON native_work_documents BEGIN
 INSERT INTO native_work_search_refresh(document_id,organization_id,project_id)
 SELECT NEW.id,organization_id,project_id FROM issues
 WHERE id=NEW.issue_id AND organization_id IS NOT NULL AND project_id IS NOT NULL
 ON CONFLICT(document_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id;
END;
-- +goose StatementEnd

DROP TRIGGER native_work_scoped_document_delete;
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_document_delete BEFORE DELETE ON native_work_documents BEGIN
 INSERT INTO native_work_search_refresh(document_id,organization_id,project_id)
 SELECT OLD.id,organization_id,project_id FROM issues
 WHERE id=OLD.issue_id AND organization_id IS NOT NULL AND project_id IS NOT NULL
 ON CONFLICT(document_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id;
 DELETE FROM native_work_scoped_fts WHERE rowid IN(OLD.id*2,OLD.id*2+1);
END;
-- +goose StatementEnd

-- +goose Down
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
