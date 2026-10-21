-- +goose Up
CREATE TABLE organizations (
 id TEXT PRIMARY KEY,
 scheduling_revision INTEGER NOT NULL DEFAULT 1,
 configuration_revision INTEGER NOT NULL DEFAULT 1,
 model_selection_json TEXT NOT NULL DEFAULT '{"enabled":true,"normal_model":"gpt-6.1-sol","complex_model":"gpt-6-astra","backend_kinds":["codex"],"default_level":"normal","levels":{"normal":{"model":"normal","effort":"high"},"complex":{"model":"normal","effort":"high"},"very_complex":{"model":"complex","effort":"medium"}},"stages":{"plan":{"issue_complexity":false,"model":"complex","effort":"low"},"code":{"issue_complexity":true},"rework":{"issue_complexity":true},"merge":{"issue_complexity":false},"validator":{"issue_complexity":false,"model":"complex","effort":"medium"},"routine":{"issue_complexity":false},"security_audit":{"issue_complexity":false}},"rules":[{"name":"very_complex","level":"very_complex","selector":{"labels":{"include":["complexity:very-complex"]}}},{"name":"complex","level":"complex","selector":{"labels":{"include":["complexity:complex"]}}}],"unavailable":"fallback","fallback_order":["normal"]}' CHECK(json_valid(model_selection_json)),
 model_selection_revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE projects (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL REFERENCES organizations(id),
 configuration_json TEXT NOT NULL CHECK(json_valid(configuration_json)),
 scheduling_rank INTEGER NOT NULL DEFAULT 2147483647,
 created_at TEXT NOT NULL,
 model_selection_json TEXT CHECK(model_selection_json IS NULL OR json_valid(model_selection_json)),
 model_selection_revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE token_grants (
 token_id TEXT NOT NULL,
 organization_id TEXT NOT NULL REFERENCES organizations(id),
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 PRIMARY KEY(token_id, organization_id, project_id)
);

-- +goose Down
DROP TABLE token_grants;
DROP TABLE projects;
DROP TABLE organizations;
