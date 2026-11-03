-- +goose Up
CREATE TABLE native_work_fingerprints (
 scope TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 document_id INTEGER NOT NULL REFERENCES native_work_documents(id) ON DELETE CASCADE,
 PRIMARY KEY (scope,fingerprint,document_id)
) WITHOUT ROWID;
CREATE INDEX native_work_fingerprints_document ON native_work_fingerprints(document_id);

-- +goose StatementBegin
CREATE TRIGGER native_work_fingerprint_insert AFTER INSERT ON native_work_documents BEGIN
 DELETE FROM native_work_fingerprints WHERE document_id IN(SELECT d.id FROM native_work_documents d WHERE d.id=NEW.id);
 INSERT INTO native_work_fingerprints(scope,fingerprint,document_id)
 SELECT scope,fingerprint,document_id FROM (
 WITH RECURSIVE keys(document_id,fingerprint,rest) AS (
 SELECT d.id,'',native_duplicate_fingerprints(c.body)
 FROM native_work_documents d JOIN native_work_search_content c ON c.id=d.id WHERE d.id=NEW.id
 UNION ALL
 SELECT document_id,substr(rest,1,instr(rest,char(10))-1),substr(rest,instr(rest,char(10))+1)
 FROM keys WHERE rest<>''
 )
 SELECT c.scope,keys.fingerprint,keys.document_id FROM keys JOIN native_work_scoped_content c ON c.source_id=keys.document_id WHERE keys.fingerprint<>''
 );
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_fingerprint_issue_update AFTER UPDATE OF body,archived,actor_principal_id ON issues BEGIN
 DELETE FROM native_work_fingerprints WHERE document_id IN(SELECT d.id FROM native_work_documents d WHERE d.issue_id=NEW.id AND d.comment_id IS NULL);
 INSERT INTO native_work_fingerprints(scope,fingerprint,document_id)
 SELECT scope,fingerprint,document_id FROM (
 WITH RECURSIVE keys(document_id,fingerprint,rest) AS (
 SELECT d.id,'',native_duplicate_fingerprints(c.body)
 FROM native_work_documents d JOIN native_work_search_content c ON c.id=d.id WHERE d.issue_id=NEW.id AND d.comment_id IS NULL
 UNION ALL
 SELECT document_id,substr(rest,1,instr(rest,char(10))-1),substr(rest,instr(rest,char(10))+1)
 FROM keys WHERE rest<>''
 )
 SELECT c.scope,keys.fingerprint,keys.document_id FROM keys JOIN native_work_scoped_content c ON c.source_id=keys.document_id WHERE keys.fingerprint<>''
 );
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_work_fingerprint_comment_update AFTER UPDATE OF body ON native_comments BEGIN
 DELETE FROM native_work_fingerprints WHERE document_id IN(SELECT d.id FROM native_work_documents d WHERE d.comment_id=NEW.id);
 INSERT INTO native_work_fingerprints(scope,fingerprint,document_id)
 SELECT scope,fingerprint,document_id FROM (
 WITH RECURSIVE keys(document_id,fingerprint,rest) AS (
 SELECT d.id,'',native_duplicate_fingerprints(c.body)
 FROM native_work_documents d JOIN native_work_search_content c ON c.id=d.id WHERE d.comment_id=NEW.id
 UNION ALL
 SELECT document_id,substr(rest,1,instr(rest,char(10))-1),substr(rest,instr(rest,char(10))+1)
 FROM keys WHERE rest<>''
 )
 SELECT c.scope,keys.fingerprint,keys.document_id FROM keys JOIN native_work_scoped_content c ON c.source_id=keys.document_id WHERE keys.fingerprint<>''
 );
END;
-- +goose StatementEnd

UPDATE native_work_search_lengths SET search_revision=-1;
UPDATE native_work_search_rebuild SET through_id=0,last_id=max(last_id,COALESCE((SELECT max(id) FROM native_work_documents),0)) WHERE id=1;

-- +goose Down
DROP TRIGGER native_work_fingerprint_insert;
DROP TRIGGER native_work_fingerprint_issue_update;
DROP TRIGGER native_work_fingerprint_comment_update;
DROP TABLE native_work_fingerprints;
