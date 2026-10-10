-- +goose Up
ALTER TABLE organization_secrets ADD COLUMN organization_slug TEXT NOT NULL DEFAULT '';

CREATE TABLE organization_sprite_pools (
 organization_id TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
 min_runners INTEGER NOT NULL DEFAULT 0 CHECK (min_runners >= 0),
 max_runners INTEGER NOT NULL DEFAULT 0 CHECK (max_runners >= min_runners AND max_runners <= 100),
 idle_seconds INTEGER NOT NULL DEFAULT 300 CHECK (idle_seconds >= 30),
 bootstrap TEXT NOT NULL DEFAULT '',
 configured_by TEXT NOT NULL REFERENCES api_tokens(id),
 revision INTEGER NOT NULL DEFAULT 1,
 isolation_tier TEXT NOT NULL DEFAULT 'native-trusted' CHECK (isolation_tier IN ('sandbox', 'native-trusted')),
 placement_json TEXT NOT NULL DEFAULT '{"mode":"blended"}' CHECK (json_valid(placement_json))
);
CREATE TABLE organization_sprite_members (
 organization_id TEXT NOT NULL REFERENCES organization_sprite_pools(organization_id) ON DELETE CASCADE,
 name TEXT NOT NULL,
 provider_organization TEXT NOT NULL,
 token_project_id TEXT NOT NULL DEFAULT '',
 enrollment_id TEXT NOT NULL REFERENCES runner_enrollments(id),
 state TEXT NOT NULL CHECK (state IN ('bootstrapping','enrolled','deleting','deleted')),
 bootstrap_log TEXT NOT NULL DEFAULT '',
 idle_since TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (organization_id,name)
);
