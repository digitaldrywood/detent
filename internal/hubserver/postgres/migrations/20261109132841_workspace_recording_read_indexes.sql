-- +goose Up
CREATE INDEX recording_workspace_subject_time ON workspace_terminal_recordings(organization_id,project_id,workspace_id,subject,started_at DESC,id DESC);
CREATE INDEX recording_workspace_relay_time ON workspace_terminal_recordings(organization_id,project_id,workspace_id,relay_session_id,started_at DESC,id DESC);
CREATE INDEX recording_workspace_subject_relay_time ON workspace_terminal_recordings(organization_id,project_id,workspace_id,subject,relay_session_id,started_at DESC,id DESC);
CREATE INDEX recording_workspace_non_user_time ON workspace_terminal_recordings(organization_id,project_id,workspace_id,started_at DESC,id DESC) WHERE isolation<>'user';
CREATE INDEX recording_workspace_non_user_relay_time ON workspace_terminal_recordings(organization_id,project_id,workspace_id,relay_session_id,started_at DESC,id DESC) WHERE isolation<>'user';

-- +goose Down
DROP INDEX recording_workspace_non_user_relay_time;
DROP INDEX recording_workspace_non_user_time;
DROP INDEX recording_workspace_subject_relay_time;
DROP INDEX recording_workspace_relay_time;
DROP INDEX recording_workspace_subject_time;
