-- +goose Up
GRANT UPDATE (outcome,target,reason,collaboration_bytes) ON scheduled_ci_decisions TO detent_runtime;

-- +goose Down
REVOKE UPDATE (outcome,target,reason,collaboration_bytes) ON scheduled_ci_decisions FROM detent_runtime;
