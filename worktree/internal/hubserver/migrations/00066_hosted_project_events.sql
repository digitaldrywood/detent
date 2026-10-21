-- +goose Up
CREATE INDEX collaboration_events_project_idx ON collaboration_events(organization_id, project_id);

-- +goose Down
DROP INDEX collaboration_events_project_idx;
