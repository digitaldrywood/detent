-- +goose Up
CREATE TABLE runner_identities_new (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  machine_id TEXT NOT NULL REFERENCES machines(id),
  token_id TEXT NOT NULL UNIQUE REFERENCES api_tokens(id),
  enrollment_id TEXT NOT NULL UNIQUE REFERENCES runner_enrollments(id),
  operations_json TEXT NOT NULL CHECK (json_valid(operations_json)),
  created_at TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  tags_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tags_json)),
  state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'draining', 'disabled')),
  capacity_limit INTEGER NOT NULL CHECK (capacity_limit >= 0),
  reported_capacity INTEGER NOT NULL CHECK (reported_capacity >= 0),
  os TEXT NOT NULL DEFAULT '',
  architecture TEXT NOT NULL DEFAULT '',
  last_heartbeat_at TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0)
);

INSERT INTO runner_identities_new (id, organization_id, machine_id, token_id, enrollment_id, operations_json, created_at, display_name, capacity_limit, reported_capacity, last_heartbeat_at)
SELECT r.id, r.organization_id, r.machine_id, r.token_id, r.enrollment_id, r.operations_json, r.created_at, m.display_name, m.capacity, m.capacity, m.last_heartbeat_at
FROM runner_identities r JOIN machines m ON m.id = r.machine_id;

DROP TABLE runner_identities;
ALTER TABLE runner_identities_new RENAME TO runner_identities;
CREATE INDEX runner_identities_host_idx ON runner_identities(machine_id, id);

ALTER TABLE machines ADD COLUMN routing_revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE runner_enrollments ADD COLUMN shared_machine INTEGER NOT NULL DEFAULT 0 CHECK (shared_machine IN (0, 1));

CREATE TABLE lease_runners (
  lease_id TEXT PRIMARY KEY REFERENCES leases(lease_id),
  runner_id TEXT NOT NULL REFERENCES runner_identities(id)
);
INSERT INTO lease_runners (lease_id, runner_id)
SELECT l.lease_id, r.id FROM leases l JOIN runner_identities r ON r.machine_id = l.machine_id;
CREATE INDEX lease_runners_runner_idx ON lease_runners(runner_id, lease_id);
