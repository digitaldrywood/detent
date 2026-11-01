-- +goose Up
DROP TRIGGER native_work_issue_insert;
DROP TRIGGER native_work_issue_update;
DROP TRIGGER native_work_issue_delete;
DROP TRIGGER native_work_comment_insert;
DROP TRIGGER native_work_comment_update;
DROP TRIGGER native_work_comment_delete;
ALTER TABLE native_work_fts RENAME TO native_work_legacy_fts;
CREATE VIRTUAL TABLE native_work_fts USING fts5(title, body, content='', contentless_delete=1, tokenize='porter unicode61');

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_insert AFTER INSERT ON issues BEGIN
 INSERT INTO native_work_documents(issue_id) VALUES(NEW.id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_delete BEFORE DELETE ON issues BEGIN
 DELETE FROM native_work_documents WHERE issue_id=OLD.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_comment_insert AFTER INSERT ON native_comments BEGIN
 INSERT INTO native_work_documents(issue_id,comment_id)
 SELECT id,NEW.id FROM issues WHERE native_id=NEW.work_item_id AND organization_id=NEW.organization_id AND project_id=NEW.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_fts_document_delete BEFORE DELETE ON native_work_documents BEGIN
 DELETE FROM native_work_fts WHERE rowid=OLD.id;
END;
-- +goose StatementEnd

UPDATE native_work_search_rebuild SET through_id=0,last_id=max(last_id,COALESCE((SELECT max(id) FROM native_work_documents),0)) WHERE id=1;

-- +goose Down
DROP TRIGGER native_work_fts_document_delete;
DROP TRIGGER native_work_issue_insert;
DROP TRIGGER native_work_issue_delete;
DROP TRIGGER native_work_comment_insert;
DROP TABLE native_work_fts;
DROP TABLE IF EXISTS native_work_legacy_fts;
CREATE VIRTUAL TABLE native_work_fts USING fts5(title, body, content='native_work_search_content', content_rowid='id', tokenize='porter unicode61');

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_insert AFTER INSERT ON issues BEGIN
  INSERT INTO native_work_documents(issue_id) VALUES (NEW.id);
  INSERT INTO native_work_fts(rowid,title,body) SELECT id,NEW.title,NEW.body FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_update AFTER UPDATE OF title,body ON issues
WHEN OLD.title IS NOT NEW.title OR OLD.body IS NOT NEW.body BEGIN
  INSERT INTO native_work_fts(native_work_fts,rowid,title,body) SELECT 'delete',id,OLD.title,OLD.body FROM native_work_documents WHERE issue_id=OLD.id AND comment_id IS NULL;
  INSERT INTO native_work_fts(rowid,title,body) SELECT id,NEW.title,NEW.body FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_delete BEFORE DELETE ON issues BEGIN
  INSERT INTO native_work_fts(native_work_fts,rowid,title,body) SELECT 'delete',id,title,body FROM native_work_search_content WHERE id IN (SELECT id FROM native_work_documents WHERE issue_id=OLD.id);
  DELETE FROM native_work_documents WHERE issue_id=OLD.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_comment_insert AFTER INSERT ON native_comments BEGIN
  INSERT INTO native_work_documents(issue_id,comment_id) SELECT id,NEW.id FROM issues WHERE native_id=NEW.work_item_id AND organization_id=NEW.organization_id AND project_id=NEW.project_id;
  INSERT INTO native_work_fts(rowid,title,body) SELECT id,'',NEW.body FROM native_work_documents WHERE comment_id=NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_comment_update AFTER UPDATE OF body ON native_comments
WHEN OLD.body IS NOT NEW.body BEGIN
  INSERT INTO native_work_fts(native_work_fts,rowid,title,body) SELECT 'delete',id,'',OLD.body FROM native_work_documents WHERE comment_id=OLD.id;
  INSERT INTO native_work_fts(rowid,title,body) SELECT id,'',NEW.body FROM native_work_documents WHERE comment_id=NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_comment_delete BEFORE DELETE ON native_comments BEGIN
  INSERT INTO native_work_fts(native_work_fts,rowid,title,body) SELECT 'delete',id,'',OLD.body FROM native_work_documents WHERE comment_id=OLD.id;
END;
-- +goose StatementEnd

INSERT INTO native_work_fts(native_work_fts) VALUES ('rebuild');
