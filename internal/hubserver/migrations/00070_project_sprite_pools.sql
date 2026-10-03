-- +goose Up
CREATE TABLE project_sprite_pools (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  min_runners INTEGER NOT NULL DEFAULT 0 CHECK (min_runners >= 0),
  max_runners INTEGER NOT NULL DEFAULT 0 CHECK (max_runners >= min_runners AND max_runners <= 100),
  idle_seconds INTEGER NOT NULL DEFAULT 300 CHECK (idle_seconds >= 30),
  bootstrap TEXT NOT NULL DEFAULT '',
  configured_by TEXT NOT NULL REFERENCES api_tokens(id),
  revision INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (organization_id, project_id),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);
CREATE TABLE project_sprite_members (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provider_organization TEXT NOT NULL,
  enrollment_id TEXT NOT NULL REFERENCES runner_enrollments(id),
  state TEXT NOT NULL CHECK (state IN ('bootstrapping', 'enrolled', 'deleting', 'deleted')),
  bootstrap_log TEXT NOT NULL DEFAULT '',
  idle_since TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, name),
  FOREIGN KEY (organization_id, project_id) REFERENCES project_sprite_pools(organization_id, project_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE project_sprite_members;
DROP TABLE project_sprite_pools;
