-- +goose Up
REVOKE DELETE ON scheduled_ci_decisions FROM detent_runtime;

-- +goose Down
GRANT DELETE ON scheduled_ci_decisions TO detent_runtime;
