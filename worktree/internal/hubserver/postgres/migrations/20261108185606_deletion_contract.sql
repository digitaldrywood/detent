-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY workspace_active_project ON workspace_sessions(organization_id,project_id) WHERE state NOT IN ('closed','failed');
CREATE INDEX CONCURRENTLY action_active_project ON project_action_runs(organization_id,project_id) WHERE status IN ('queued','running');
CREATE INDEX CONCURRENTLY conversation_active_project ON conversations(organization_id,project_id) WHERE execution_status IS NULL OR execution_lease_id IS NULL OR execution_status IN ('waiting_for_runner','starting','running','waiting_input','interrupting','unknown');
CREATE INDEX CONCURRENTLY conversation_message_active ON conversation_messages(organization_id,conversation_id) WHERE delivery IN ('queued','sending','sent','delivered','responding','unknown');
CREATE INDEX CONCURRENTLY github_import_active_project ON github_imports(organization_id,project_id) WHERE status IN ('pending','running','partial') OR intake_pending;
CREATE INDEX CONCURRENTLY github_outbound_active_project ON github_outbound(organization_id,project_id) WHERE status IN ('pending','processing','retrying');
CREATE INDEX CONCURRENTLY sprite_active_token_project ON runner_sprite_memberships(organization_id,token_project_id) WHERE state<>'deleted';
CREATE INDEX CONCURRENTLY artifact_authorization_active_project ON artifact_upload_authorizations(organization_id,project_id,expires_at);
CREATE INDEX CONCURRENTLY slack_delivery_active_finding ON health_slack_deliveries(organization_id,finding_id) WHERE completed_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY slack_delivery_active_finding;
DROP INDEX CONCURRENTLY artifact_authorization_active_project;
DROP INDEX CONCURRENTLY sprite_active_token_project;
DROP INDEX CONCURRENTLY github_outbound_active_project;
DROP INDEX CONCURRENTLY github_import_active_project;
DROP INDEX CONCURRENTLY conversation_message_active;
DROP INDEX CONCURRENTLY conversation_active_project;
DROP INDEX CONCURRENTLY action_active_project;
DROP INDEX CONCURRENTLY workspace_active_project;
