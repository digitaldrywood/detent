-- +goose Up
CREATE INDEX issues_native_search_scope ON issues(organization_id,project_id,archived,id);
CREATE TABLE native_work_search_lengths (
 document_id INTEGER NOT NULL REFERENCES native_work_documents(id) ON DELETE CASCADE,
 scope TEXT NOT NULL,
 tokens INTEGER NOT NULL,
 PRIMARY KEY(document_id,scope)
);
CREATE TABLE native_work_search_population (
 scope TEXT PRIMARY KEY,
 documents INTEGER NOT NULL,
 tokens INTEGER NOT NULL
);
CREATE TABLE native_work_search_postings (
 scope TEXT NOT NULL,
 term TEXT NOT NULL,
 document_id INTEGER NOT NULL,
 frequency INTEGER NOT NULL,
 impact REAL NOT NULL,
 PRIMARY KEY(document_id,scope,term),
 FOREIGN KEY(document_id,scope) REFERENCES native_work_search_lengths(document_id,scope) ON DELETE CASCADE
);
CREATE INDEX native_work_search_impact ON native_work_search_postings(scope,term,impact DESC,document_id);
CREATE TABLE native_work_search_frequencies (
 scope TEXT NOT NULL,
 term TEXT NOT NULL,
 documents INTEGER NOT NULL,
 PRIMARY KEY(scope,term)
);
CREATE TABLE native_work_search_refresh (
 document_id INTEGER PRIMARY KEY REFERENCES native_work_documents(id) ON DELETE CASCADE
);

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

-- +goose StatementBegin
CREATE TRIGGER native_work_search_posting_insert AFTER INSERT ON native_work_search_postings BEGIN
 INSERT INTO native_work_search_frequencies VALUES(NEW.scope,NEW.term,1)
 ON CONFLICT(scope,term) DO UPDATE SET documents=documents+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_search_posting_delete AFTER DELETE ON native_work_search_postings BEGIN
 UPDATE native_work_search_frequencies SET documents=documents-1 WHERE scope=OLD.scope AND term=OLD.term;
 DELETE FROM native_work_search_frequencies WHERE scope=OLD.scope AND term=OLD.term AND documents=0;
END;
-- +goose StatementEnd

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

UPDATE native_work_search_rebuild SET through_id=0,last_id=COALESCE((SELECT max(id) FROM native_work_documents),0) WHERE id=1;

-- +goose Down
DROP INDEX issues_native_search_scope;
DROP TRIGGER native_work_search_comment_update;
DROP TRIGGER native_work_search_issue_update;
DROP TRIGGER native_work_search_document_insert;
DROP TRIGGER native_work_search_refresh_insert;
DROP TRIGGER native_work_search_posting_delete;
DROP TRIGGER native_work_search_posting_insert;
DROP TRIGGER native_work_search_length_delete;
DROP TRIGGER native_work_search_length_insert;
DROP TABLE native_work_search_refresh;
DROP TABLE native_work_search_frequencies;
DROP TABLE native_work_search_postings;
DROP TABLE native_work_search_population;
DROP TABLE native_work_search_lengths;
