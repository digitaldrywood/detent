-- +goose Up
CREATE INDEX issues_billing_current_population_idx ON issues (organization_id,project_id,workflow_state_name,id) WHERE NOT archived;

-- +goose Down
DROP INDEX issues_billing_current_population_idx;
