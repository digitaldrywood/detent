-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_project_configurations VALIDATE CONSTRAINT runner_configuration_request_status;
ALTER TABLE runner_project_configurations VALIDATE CONSTRAINT runner_configuration_request_payload;
ALTER TABLE runner_project_configurations VALIDATE CONSTRAINT runner_configuration_observation_payload;

-- +goose Down
SELECT 1;
