-- +goose Up
GRANT DELETE ON scheduled_ci_decisions TO detent_runtime;

-- +goose Down
REVOKE DELETE ON scheduled_ci_decisions FROM detent_runtime;
