-- +goose Up
ALTER TABLE organizations ADD COLUMN model_selection_json TEXT NOT NULL DEFAULT '{"enabled":true,"normal_model":"gpt-6.1-sol","complex_model":"gpt-6-astra","backend_kinds":["codex"],"default_level":"normal","levels":{"normal":{"model":"normal","effort":"high"},"complex":{"model":"normal","effort":"high"},"very_complex":{"model":"complex","effort":"medium"}},"stages":{"plan":{"issue_complexity":false,"model":"complex","effort":"low"},"code":{"issue_complexity":true},"rework":{"issue_complexity":true},"merge":{"issue_complexity":false},"validator":{"issue_complexity":false,"model":"complex","effort":"medium"},"routine":{"issue_complexity":false},"security_audit":{"issue_complexity":false}},"rules":[{"name":"very_complex","level":"very_complex","selector":{"labels":{"include":["complexity:very-complex"]}}},{"name":"complex","level":"complex","selector":{"labels":{"include":["complexity:complex"]}}}],"unavailable":"fallback","fallback_order":["normal"]}' CHECK (json_valid(model_selection_json));
ALTER TABLE organizations ADD COLUMN model_selection_revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE projects ADD COLUMN model_selection_json TEXT CHECK (model_selection_json IS NULL OR json_valid(model_selection_json));
ALTER TABLE projects ADD COLUMN model_selection_revision INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE projects DROP COLUMN model_selection_revision;
ALTER TABLE projects DROP COLUMN model_selection_json;
ALTER TABLE organizations DROP COLUMN model_selection_revision;
ALTER TABLE organizations DROP COLUMN model_selection_json;
