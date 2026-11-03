-- +goose Up
CREATE TABLE native_work_fingerprints (
 scope TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 document_id INTEGER NOT NULL REFERENCES native_work_documents(id) ON DELETE CASCADE,
 PRIMARY KEY (scope,fingerprint,document_id)
) WITHOUT ROWID;
CREATE INDEX native_work_fingerprints_document ON native_work_fingerprints(document_id);

UPDATE native_work_search_rebuild SET through_id=0,last_id=max(last_id,COALESCE((SELECT max(id) FROM native_work_documents),0)) WHERE id=1;

-- +goose Down
DROP TABLE native_work_fingerprints;
