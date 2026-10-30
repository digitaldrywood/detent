-- +goose Up
DROP TABLE native_work_scoped_fts;
CREATE VIRTUAL TABLE native_work_scoped_fts USING fts5(title,body,scope,content='',contentless_delete=1,tokenize='native_scoped');
UPDATE native_work_search_rebuild SET through_id=0,last_id=COALESCE((SELECT max(id) FROM native_work_documents),0) WHERE id=1;

-- +goose Down
DROP TABLE native_work_scoped_fts;
CREATE VIRTUAL TABLE native_work_scoped_fts USING fts5(title,body,scope,content='',contentless_delete=1,tokenize='porter unicode61');
UPDATE native_work_search_rebuild SET through_id=0,last_id=COALESCE((SELECT max(id) FROM native_work_documents),0) WHERE id=1;
