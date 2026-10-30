-- +goose Up
CREATE TABLE dashboard_delivery_authorization (sequence INTEGER NOT NULL);
INSERT INTO dashboard_delivery_authorization(sequence) VALUES(0);

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_api_tokens_insert AFTER INSERT ON api_tokens
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_api_tokens_update AFTER UPDATE OF token_hash,scope,revoked_at,expires_at,native_only,hosted_user_id,hosted_organization_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind ON api_tokens
WHEN NEW.token_hash IS NOT OLD.token_hash OR NEW.scope IS NOT OLD.scope OR NEW.revoked_at IS NOT OLD.revoked_at OR NEW.expires_at IS NOT OLD.expires_at OR NEW.native_only IS NOT OLD.native_only OR NEW.hosted_user_id IS NOT OLD.hosted_user_id OR NEW.hosted_organization_id IS NOT OLD.hosted_organization_id OR NEW.hosted_membership_id IS NOT OLD.hosted_membership_id OR NEW.operator_key_scope IS NOT OLD.operator_key_scope OR NEW.operator_project_access IS NOT OLD.operator_project_access OR NEW.key_kind IS NOT OLD.key_kind
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_api_tokens_delete AFTER DELETE ON api_tokens
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_members_insert AFTER INSERT ON hosted_members
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_members_update AFTER UPDATE OF user_id,membership_id,role,active,principal_id ON hosted_members
WHEN NEW.user_id IS NOT OLD.user_id OR NEW.membership_id IS NOT OLD.membership_id OR NEW.role IS NOT OLD.role OR NEW.active IS NOT OLD.active OR NEW.principal_id IS NOT OLD.principal_id
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_members_delete AFTER DELETE ON hosted_members
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_project_grants_insert AFTER INSERT ON hosted_project_grants
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_project_grants_update AFTER UPDATE OF user_id,organization_id,project_id,can_write,manage_runner ON hosted_project_grants
WHEN NEW.user_id IS NOT OLD.user_id OR NEW.organization_id IS NOT OLD.organization_id OR NEW.project_id IS NOT OLD.project_id OR NEW.can_write IS NOT OLD.can_write OR NEW.manage_runner IS NOT OLD.manage_runner
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_project_grants_delete AFTER DELETE ON hosted_project_grants
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_sessions_insert AFTER INSERT ON hosted_sessions
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_sessions_update AFTER UPDATE OF token_hash,identity_json,expires_at,revoked_at ON hosted_sessions
WHEN NEW.token_hash IS NOT OLD.token_hash OR NEW.identity_json IS NOT OLD.identity_json OR NEW.expires_at IS NOT OLD.expires_at OR NEW.revoked_at IS NOT OLD.revoked_at
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_sessions_delete AFTER DELETE ON hosted_sessions
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_tenant_insert AFTER INSERT ON hosted_tenant
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_tenant_update AFTER UPDATE OF organization_id,allocation_generation,deployment ON hosted_tenant
WHEN NEW.organization_id IS NOT OLD.organization_id OR NEW.allocation_generation IS NOT OLD.allocation_generation OR NEW.deployment IS NOT OLD.deployment
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_hosted_tenant_delete AFTER DELETE ON hosted_tenant
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_token_grants_insert AFTER INSERT ON token_grants
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_token_grants_update AFTER UPDATE OF token_id,organization_id,project_id ON token_grants
WHEN NEW.token_id IS NOT OLD.token_id OR NEW.organization_id IS NOT OLD.organization_id OR NEW.project_id IS NOT OLD.project_id
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_token_grants_delete AFTER DELETE ON token_grants
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_projects_update AFTER UPDATE OF organization_id,deleted_at ON projects
WHEN NEW.organization_id IS NOT OLD.organization_id OR NEW.deleted_at IS NOT OLD.deleted_at
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_authorization_issues_update AFTER UPDATE OF actor_json ON issues
WHEN NEW.actor_json IS NOT OLD.actor_json
BEGIN
 UPDATE dashboard_delivery_authorization SET sequence=sequence+1;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER dashboard_authorization_api_tokens_insert;
DROP TRIGGER dashboard_authorization_api_tokens_update;
DROP TRIGGER dashboard_authorization_api_tokens_delete;
DROP TRIGGER dashboard_authorization_hosted_members_insert;
DROP TRIGGER dashboard_authorization_hosted_members_update;
DROP TRIGGER dashboard_authorization_hosted_members_delete;
DROP TRIGGER dashboard_authorization_hosted_project_grants_insert;
DROP TRIGGER dashboard_authorization_hosted_project_grants_update;
DROP TRIGGER dashboard_authorization_hosted_project_grants_delete;
DROP TRIGGER dashboard_authorization_hosted_sessions_insert;
DROP TRIGGER dashboard_authorization_hosted_sessions_update;
DROP TRIGGER dashboard_authorization_hosted_sessions_delete;
DROP TRIGGER dashboard_authorization_hosted_tenant_insert;
DROP TRIGGER dashboard_authorization_hosted_tenant_update;
DROP TRIGGER dashboard_authorization_hosted_tenant_delete;
DROP TRIGGER dashboard_authorization_token_grants_insert;
DROP TRIGGER dashboard_authorization_token_grants_update;
DROP TRIGGER dashboard_authorization_token_grants_delete;
DROP TRIGGER dashboard_authorization_projects_update;
DROP TRIGGER dashboard_authorization_issues_update;
DROP TABLE dashboard_delivery_authorization;
