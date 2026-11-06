-- +goose Up
CREATE TABLE organization_sprite_pools_expanded (
 organization_id TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
 min_runners INTEGER NOT NULL DEFAULT 0 CHECK (min_runners >= 0),
 max_runners INTEGER NOT NULL DEFAULT 0 CHECK (max_runners >= min_runners),
 idle_seconds INTEGER NOT NULL DEFAULT 300 CHECK (idle_seconds >= 30),
 bootstrap TEXT NOT NULL DEFAULT '',
 configured_by TEXT NOT NULL REFERENCES api_tokens(id),
 revision INTEGER NOT NULL DEFAULT 1,
 isolation_tier TEXT NOT NULL DEFAULT 'native-trusted' CHECK (isolation_tier IN ('sandbox', 'native-trusted')),
 placement_json TEXT NOT NULL DEFAULT '{"mode":"blended"}' CHECK (json_valid(placement_json))
);
INSERT INTO organization_sprite_pools_expanded SELECT * FROM organization_sprite_pools;
DROP TABLE organization_sprite_pools;
ALTER TABLE organization_sprite_pools_expanded RENAME TO organization_sprite_pools;
