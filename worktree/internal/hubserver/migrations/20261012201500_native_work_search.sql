-- +goose Up
CREATE TABLE native_work_documents (
  id INTEGER PRIMARY KEY,
  issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  comment_id TEXT UNIQUE REFERENCES native_comments(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  body TEXT NOT NULL
);
CREATE UNIQUE INDEX native_work_document_issue ON native_work_documents(issue_id) WHERE comment_id IS NULL;
CREATE INDEX native_work_document_comments ON native_work_documents(issue_id);
CREATE VIRTUAL TABLE native_work_fts USING fts5(title, body, content='native_work_documents', content_rowid='id', tokenize='porter unicode61');

-- +goose StatementBegin
CREATE TRIGGER native_work_document_insert AFTER INSERT ON native_work_documents BEGIN
  INSERT INTO native_work_fts(rowid, title, body) VALUES (NEW.id, NEW.title, NEW.body);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_document_delete AFTER DELETE ON native_work_documents BEGIN
  INSERT INTO native_work_fts(native_work_fts, rowid, title, body) VALUES ('delete', OLD.id, OLD.title, OLD.body);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_document_update AFTER UPDATE ON native_work_documents BEGIN
  INSERT INTO native_work_fts(native_work_fts, rowid, title, body) VALUES ('delete', OLD.id, OLD.title, OLD.body);
  INSERT INTO native_work_fts(rowid, title, body) VALUES (NEW.id, NEW.title, NEW.body);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_insert AFTER INSERT ON issues BEGIN
  INSERT INTO native_work_documents(issue_id, title, body) VALUES (NEW.id, NEW.title, NEW.body);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_issue_update AFTER UPDATE OF title, body ON issues
WHEN OLD.title IS NOT NEW.title OR OLD.body IS NOT NEW.body BEGIN
  UPDATE native_work_documents SET title = NEW.title, body = NEW.body WHERE issue_id = NEW.id AND comment_id IS NULL;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_comment_insert AFTER INSERT ON native_comments BEGIN
  INSERT INTO native_work_documents(issue_id, comment_id, title, body) SELECT id, NEW.id, '', NEW.body FROM issues WHERE native_id = NEW.work_item_id AND organization_id = NEW.organization_id AND project_id = NEW.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_comment_update AFTER UPDATE OF body ON native_comments
WHEN OLD.body IS NOT NEW.body BEGIN
  UPDATE native_work_documents SET body = NEW.body WHERE comment_id = NEW.id;
END;
-- +goose StatementEnd

INSERT INTO native_work_documents(issue_id, title, body) SELECT id, title, body FROM issues;
INSERT INTO native_work_documents(issue_id, comment_id, title, body)
SELECT i.id, c.id, '', c.body FROM native_comments c JOIN issues i
ON i.native_id = c.work_item_id AND i.organization_id = c.organization_id AND i.project_id = c.project_id;

-- +goose Down
DROP TRIGGER native_work_issue_insert;
DROP TRIGGER native_work_issue_update;
DROP TRIGGER native_work_comment_insert;
DROP TRIGGER native_work_comment_update;
DROP TRIGGER native_work_document_insert;
DROP TRIGGER native_work_document_update;
DROP TRIGGER native_work_document_delete;
DROP TABLE native_work_fts;
DROP TABLE native_work_documents;
