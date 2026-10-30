-- +goose Up
UPDATE native_work_search_rebuild SET through_id=0,last_id=COALESCE((SELECT max(id) FROM native_work_documents),0) WHERE id=1;

-- +goose Down
UPDATE native_work_search_rebuild SET through_id=0,last_id=COALESCE((SELECT max(id) FROM native_work_documents),0) WHERE id=1;
