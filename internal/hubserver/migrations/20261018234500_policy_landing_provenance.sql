-- +goose Up
ALTER TABLE project_workflow_applies ADD COLUMN change_id TEXT NOT NULL DEFAULT '';
ALTER TABLE project_workflow_applies ADD COLUMN pull_request_url TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN merge_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN merged_at TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_requests ADD COLUMN merged_by TEXT NOT NULL DEFAULT '';
CREATE INDEX pull_requests_landing_idx ON pull_requests(repository_id, merge_sha, base_ref);

CREATE INDEX change_requests_policy_landing_idx ON change_requests(organization_id, project_id, json_extract(record_json, '$.landed.merge_sha'), json_extract(record_json, '$.landed.base_ref'));
CREATE INDEX project_workflow_applies_revision_idx ON project_workflow_applies(source_commit, scope);

-- +goose Down
DROP INDEX project_workflow_applies_revision_idx;
DROP INDEX change_requests_policy_landing_idx;
DROP INDEX pull_requests_landing_idx;
ALTER TABLE pull_requests DROP COLUMN merged_by;
ALTER TABLE pull_requests DROP COLUMN merged_at;
ALTER TABLE pull_requests DROP COLUMN merge_sha;
ALTER TABLE project_workflow_applies DROP COLUMN pull_request_url;
ALTER TABLE project_workflow_applies DROP COLUMN change_id;
