-- +goose Up
-- +goose StatementBegin
CREATE INDEX collaboration_events_version_landing_idx ON collaboration_events (organization_id,project_id,work_item_id,landing_change_id,landing_version_id,landing_head_sha,sequence DESC) WHERE landing_version_id<>'';
CREATE INDEX collaboration_events_work_item_type_idx ON collaboration_events (organization_id,project_id,work_item_id,type,sequence DESC);
CREATE INDEX collaboration_events_version_transition_idx ON collaboration_events (organization_id,project_id,work_item_id,change_id,version_id,head_sha,sequence DESC) WHERE type='workflow.transitioned';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX collaboration_events_version_transition_idx;
DROP INDEX collaboration_events_work_item_type_idx;
DROP INDEX collaboration_events_version_landing_idx;
-- +goose StatementEnd
