-- +goose Up
DROP TRIGGER native_work_search_refresh_insert;
DROP TRIGGER native_work_search_document_insert;
DROP TRIGGER native_work_search_issue_update;
DROP TRIGGER native_work_search_comment_update;
ALTER TABLE native_work_search_refresh ADD COLUMN organization_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_work_search_refresh ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
CREATE INDEX native_work_search_refresh_scope ON native_work_search_refresh(organization_id,project_id,document_id);
ALTER TABLE native_work_documents ADD COLUMN search_revision INTEGER NOT NULL DEFAULT 0;

-- +goose StatementBegin
CREATE TRIGGER native_work_search_document_insert AFTER INSERT ON native_work_documents BEGIN
 INSERT OR IGNORE INTO native_work_search_refresh SELECT NEW.id,organization_id,project_id FROM issues WHERE id=NEW.issue_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_issue_update AFTER UPDATE OF title,body,organization_id,project_id,archived,actor_principal_id ON issues BEGIN
 UPDATE native_work_documents SET search_revision=search_revision+1 WHERE issue_id=NEW.id AND comment_id IS NULL;
 INSERT INTO native_work_search_refresh SELECT id,NEW.organization_id,NEW.project_id FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL
 ON CONFLICT(document_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_comment_update AFTER UPDATE OF body ON native_comments WHEN OLD.body IS NOT NEW.body BEGIN
 UPDATE native_work_documents SET search_revision=search_revision+1 WHERE comment_id=NEW.id;
 INSERT OR IGNORE INTO native_work_search_refresh SELECT id,NEW.organization_id,NEW.project_id FROM native_work_documents WHERE comment_id=NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER native_work_search_document_insert;
DROP TRIGGER native_work_search_issue_update;
DROP TRIGGER native_work_search_comment_update;
DROP INDEX native_work_search_refresh_scope;
ALTER TABLE native_work_search_refresh DROP COLUMN organization_id;
ALTER TABLE native_work_search_refresh DROP COLUMN project_id;
ALTER TABLE native_work_documents DROP COLUMN search_revision;

-- +goose StatementBegin
CREATE TRIGGER native_work_search_refresh_insert AFTER INSERT ON native_work_search_refresh BEGIN
 DELETE FROM native_work_search_lengths WHERE document_id=NEW.document_id;
 INSERT INTO native_work_search_lengths(document_id,scope,tokens)
 SELECT NEW.document_id,s.scope,native_search_tokens(c.title,'')+native_search_tokens(c.body,'')
 FROM native_work_search_content c JOIN native_work_scoped_content s ON s.source_id=c.id
 WHERE c.id=NEW.document_id;
 INSERT INTO native_work_search_postings(scope,term,document_id,frequency,impact)
 SELECT l.scope,substr(record,instr(record,':')+1),NEW.document_id,
 CAST(substr(record,1,instr(record,':')-1) AS INTEGER),
 CAST(substr(record,1,instr(record,':')-1) AS REAL)/(1+l.tokens)
 FROM (
 WITH RECURSIVE records(record,rest) AS (
 SELECT '',native_search_postings(title,body) FROM native_work_search_content WHERE id=NEW.document_id
 UNION ALL SELECT substr(rest,1,instr(rest,' ')-1),substr(rest,instr(rest,' ')+1) FROM records WHERE rest<>''
 ) SELECT record FROM records WHERE record<>''
 ) CROSS JOIN native_work_search_lengths l WHERE l.document_id=NEW.document_id;
 DELETE FROM native_work_search_refresh WHERE document_id=NEW.document_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_document_insert AFTER INSERT ON native_work_documents BEGIN
 INSERT INTO native_work_search_refresh VALUES(NEW.id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_issue_update AFTER UPDATE OF title,body,organization_id,project_id,archived,actor_principal_id ON issues BEGIN
 INSERT INTO native_work_search_refresh SELECT id FROM native_work_documents WHERE issue_id=NEW.id AND comment_id IS NULL;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_comment_update AFTER UPDATE OF body ON native_comments WHEN OLD.body IS NOT NEW.body BEGIN
 INSERT INTO native_work_search_refresh SELECT id FROM native_work_documents WHERE comment_id=NEW.id;
END;
-- +goose StatementEnd
