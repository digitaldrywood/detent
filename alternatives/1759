-- +goose Up
DROP INDEX change_requests_activity_latest;
CREATE INDEX change_requests_activity_latest ON change_requests(organization_id,project_id,work_item_id,activity_created_at DESC,id DESC);

-- +goose Down
DROP INDEX change_requests_activity_latest;
CREATE INDEX change_requests_activity_latest ON change_requests(organization_id,project_id,work_item_id,created_at DESC,id DESC);
