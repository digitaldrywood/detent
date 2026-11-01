-- +goose Up
CREATE INDEX runner_capacity_lease_window ON leases(
  machine_id,
  min(julianday(coalesce(released_at,expires_at)),julianday(expires_at)),
  julianday(acquired_at)
);

-- +goose Down
DROP INDEX runner_capacity_lease_window;
