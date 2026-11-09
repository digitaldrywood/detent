-- +goose Up
CREATE INDEX issues_actor_workspaces ON issues(organization_id,project_id,actor_principal_id,native_id);
CREATE INDEX action_run_action_workspace_recent ON project_action_runs(organization_id,project_id,action_id,workspace_id,created_at DESC,id DESC);

-- +goose Down
DROP INDEX action_run_action_workspace_recent;
DROP INDEX issues_actor_workspaces;
