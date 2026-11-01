-- +goose Up
CREATE INDEX collaboration_events_health_transition_unknown ON collaboration_events(organization_id,id) WHERE type='workflow.transitioned' AND health_projected IS NULL;

-- +goose Down
DROP INDEX collaboration_events_health_transition_unknown;
