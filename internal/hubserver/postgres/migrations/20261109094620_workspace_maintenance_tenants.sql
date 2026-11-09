-- +goose Up
GRANT SELECT(organization_id,state) ON workspace_sessions TO detent_platform;
GRANT SELECT(organization_id,status) ON project_action_runs TO detent_platform;
GRANT SELECT(organization_id) ON workspace_relay_tickets TO detent_platform;
CREATE POLICY workspace_maintenance_metadata ON workspace_sessions FOR SELECT TO detent_platform USING(true);
CREATE POLICY workspace_maintenance_metadata ON project_action_runs FOR SELECT TO detent_platform USING(true);
CREATE POLICY workspace_maintenance_metadata ON workspace_relay_tickets FOR SELECT TO detent_platform USING(true);

-- +goose Down
DROP POLICY workspace_maintenance_metadata ON workspace_relay_tickets;
DROP POLICY workspace_maintenance_metadata ON project_action_runs;
DROP POLICY workspace_maintenance_metadata ON workspace_sessions;
REVOKE SELECT(organization_id) ON workspace_relay_tickets FROM detent_platform;
REVOKE SELECT(organization_id,status) ON project_action_runs FROM detent_platform;
REVOKE SELECT(organization_id,state) ON workspace_sessions FROM detent_platform;
