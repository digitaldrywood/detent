-- +goose Up
CREATE VIRTUAL TABLE native_work_scoped_fts USING fts5(title,body,scope,content='',contentless_delete=1,tokenize='porter unicode61');
CREATE VIEW native_work_scoped_content AS
SELECT source_id,source_id*2+variant AS id,native_search_text(title,scope) AS title,native_search_text(body,scope) AS body,scope FROM (
 SELECT c.id AS source_id,0 AS variant,c.title,c.body,
 's'||hex(i.organization_id)||'z'||hex(i.project_id)||'a'||i.archived||'v' AS scope
 FROM native_work_search_content c JOIN native_work_documents d ON d.id=c.id JOIN issues i ON i.id=d.issue_id
 UNION ALL
 SELECT c.id,1,c.title,c.body,
 's'||hex(i.organization_id)||'z'||hex(i.project_id)||'a'||i.archived||'r'||hex(COALESCE(i.actor_principal_id,''))||'v'
 FROM native_work_search_content c JOIN native_work_documents d ON d.id=c.id JOIN issues i ON i.id=d.issue_id
);

CREATE TABLE native_work_search_rebuild (
 id INTEGER PRIMARY KEY CHECK(id=1),
 through_id INTEGER NOT NULL,
 last_id INTEGER NOT NULL
);
INSERT INTO native_work_search_rebuild VALUES(1,0,COALESCE((SELECT max(id) FROM native_work_documents),0));
CREATE TABLE native_work_search_dirty (
 issue_id INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 through_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX native_work_search_dirty_scope ON native_work_search_dirty(organization_id,project_id);

-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_document_insert AFTER INSERT ON native_work_documents BEGIN
 INSERT INTO native_work_scoped_fts(rowid,title,body,scope) SELECT id,title,body,scope FROM native_work_scoped_content WHERE source_id=NEW.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_document_delete BEFORE DELETE ON native_work_documents BEGIN
 DELETE FROM native_work_scoped_fts WHERE rowid IN(OLD.id*2,OLD.id*2+1);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_issue_update AFTER UPDATE OF title,body,organization_id,project_id,archived,actor_principal_id ON issues BEGIN
 DELETE FROM native_work_scoped_fts WHERE rowid IN(SELECT id*2 FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL UNION ALL SELECT id*2+1 FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL);
 INSERT INTO native_work_scoped_fts(rowid,title,body,scope) SELECT c.id,c.title,c.body,c.scope FROM native_work_scoped_content c
 WHERE c.source_id=(SELECT id FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL);
 INSERT INTO native_work_search_dirty(issue_id,organization_id,project_id)
 SELECT NEW.id,NEW.organization_id,NEW.project_id WHERE
 (OLD.organization_id IS NOT NEW.organization_id OR OLD.project_id IS NOT NEW.project_id OR OLD.archived IS NOT NEW.archived OR OLD.actor_principal_id IS NOT NEW.actor_principal_id)
 AND EXISTS(SELECT 1 FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NOT NULL)
 ON CONFLICT(issue_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id,through_id=0;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER native_work_scoped_comment_update AFTER UPDATE OF body ON native_comments WHEN OLD.body IS NOT NEW.body BEGIN
 DELETE FROM native_work_scoped_fts WHERE rowid IN(SELECT id*2 FROM native_work_documents WHERE comment_id=NEW.id UNION ALL SELECT id*2+1 FROM native_work_documents WHERE comment_id=NEW.id);
 INSERT INTO native_work_scoped_fts(rowid,title,body,scope) SELECT c.id,c.title,c.body,c.scope FROM native_work_scoped_content c WHERE c.source_id=(SELECT id FROM native_work_documents WHERE comment_id=NEW.id);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER native_work_scoped_document_insert;
DROP TRIGGER native_work_scoped_document_delete;
DROP TRIGGER native_work_scoped_issue_update;
DROP TRIGGER native_work_scoped_comment_update;
DROP VIEW native_work_scoped_content;
DROP TABLE native_work_scoped_fts;
DROP TABLE native_work_search_dirty;
DROP TABLE native_work_search_rebuild;
