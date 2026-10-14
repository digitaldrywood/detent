-- +goose Up
ALTER TABLE projects ADD COLUMN scheduling_rank INTEGER NOT NULL DEFAULT 2147483647;
ALTER TABLE organizations ADD COLUMN scheduling_revision INTEGER NOT NULL DEFAULT 1;
UPDATE projects SET scheduling_rank = (
 SELECT count(*) FROM projects older WHERE older.organization_id = projects.organization_id
 AND (older.created_at < projects.created_at OR (older.created_at = projects.created_at AND older.id < projects.id))
);
UPDATE runner_identities SET routing_settings_json = json_remove(routing_settings_json, '$.home_project_ids', '$.spillover');
UPDATE runner_identities SET problems_json = COALESCE((
 SELECT json_group_array(json(value)) FROM json_each(runner_identities.problems_json)
 WHERE json_extract(value, '$.code') <> 'home_project_unservable'
), '[]');
ALTER TABLE runner_identities DROP COLUMN home_dry_since;

-- +goose Down
ALTER TABLE runner_identities ADD COLUMN home_dry_since TEXT;
ALTER TABLE organizations DROP COLUMN scheduling_revision;
ALTER TABLE projects DROP COLUMN scheduling_rank;
