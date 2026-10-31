-- +goose Up
PRAGMA legacy_alter_table = ON;

CREATE TABLE native_work_search_lengths_deferred (
 document_id INTEGER NOT NULL,
 scope TEXT NOT NULL,
 tokens INTEGER NOT NULL,
 search_revision INTEGER NOT NULL DEFAULT -1,
 PRIMARY KEY(document_id,scope)
);
INSERT INTO native_work_search_lengths_deferred SELECT document_id,scope,tokens,search_revision FROM native_work_search_lengths;
DROP TABLE native_work_search_lengths;
ALTER TABLE native_work_search_lengths_deferred RENAME TO native_work_search_lengths;

-- +goose StatementBegin
CREATE TRIGGER native_work_search_length_insert AFTER INSERT ON native_work_search_lengths BEGIN
 INSERT INTO native_work_search_population VALUES(NEW.scope,1,NEW.tokens)
 ON CONFLICT(scope) DO UPDATE SET documents=documents+1,tokens=tokens+NEW.tokens;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_length_delete AFTER DELETE ON native_work_search_lengths BEGIN
 UPDATE native_work_search_population SET documents=documents-1,tokens=tokens-OLD.tokens WHERE scope=OLD.scope;
 DELETE FROM native_work_search_population WHERE scope=OLD.scope AND documents=0;
END;
-- +goose StatementEnd

CREATE TABLE native_work_search_refresh_deferred (
 document_id INTEGER PRIMARY KEY,
 organization_id TEXT NOT NULL DEFAULT '',
 project_id TEXT NOT NULL DEFAULT ''
);
INSERT INTO native_work_search_refresh_deferred SELECT document_id,organization_id,project_id FROM native_work_search_refresh;
DROP TABLE native_work_search_refresh;
ALTER TABLE native_work_search_refresh_deferred RENAME TO native_work_search_refresh;
CREATE INDEX native_work_search_refresh_scope ON native_work_search_refresh(organization_id,project_id,document_id);

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

PRAGMA legacy_alter_table = OFF;

-- +goose Down
SELECT 1;
