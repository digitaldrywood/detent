-- +goose Up
ALTER TABLE project_action_runs ADD COLUMN claimed_stream_id text NOT NULL DEFAULT '';
ALTER TABLE workspace_sessions DROP CONSTRAINT workspace_sessions_ref_check;
CREATE INDEX workspace_project_recent ON workspace_sessions(organization_id,project_id,created_at DESC,id DESC);
CREATE INDEX workspace_project_state_recent ON workspace_sessions(organization_id,project_id,state,created_at DESC,id DESC);
CREATE INDEX workspace_subject_state_recent ON workspace_sessions(organization_id,project_id,subject_work_item_id,state,created_at DESC,id DESC);
CREATE INDEX workspace_open_actor ON workspace_sessions(organization_id,created_by) WHERE state NOT IN ('closed','failed');
CREATE INDEX workspace_open_scan ON workspace_sessions(organization_id,id) WHERE state NOT IN ('closed','failed');
CREATE INDEX action_run_recent ON project_action_runs(organization_id,project_id,action_id,created_at DESC,id DESC);
CREATE INDEX action_run_workspace_queue ON project_action_runs(organization_id,project_id,workspace_id,created_at,id) WHERE status='queued';
CREATE INDEX action_run_active_scan ON project_action_runs(organization_id,id) WHERE status IN ('queued','running');

-- +goose Down
ALTER TABLE project_action_runs DROP COLUMN claimed_stream_id;
DROP INDEX action_run_active_scan;
DROP INDEX action_run_workspace_queue;
DROP INDEX action_run_recent;
DROP INDEX workspace_open_scan;
DROP INDEX workspace_open_actor;
DROP INDEX workspace_subject_state_recent;
DROP INDEX workspace_project_state_recent;
DROP INDEX workspace_project_recent;
ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_ref_check CHECK (btrim(ref)<>'');
