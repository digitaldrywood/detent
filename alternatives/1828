-- +goose Up
INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner)
SELECT m.user_id,p.organization_id,p.id,1,1
FROM hosted_members m CROSS JOIN projects p
JOIN hosted_tenant h ON h.organization_id=p.organization_id
WHERE m.active=1 AND m.role IN ('owner','admin')
ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=1,manage_runner=1;

INSERT INTO token_grants(token_id,organization_id,project_id)
SELECT m.principal_id,p.organization_id,p.id
FROM hosted_members m CROSS JOIN projects p
JOIN hosted_tenant h ON h.organization_id=p.organization_id
WHERE m.active=1 AND m.role IN ('owner','admin')
ON CONFLICT DO NOTHING;

-- +goose StatementBegin
CREATE TRIGGER projects_hosted_administrator_grants AFTER INSERT ON projects
BEGIN
  INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner)
  SELECT m.user_id,NEW.organization_id,NEW.id,1,1
  FROM hosted_members m JOIN hosted_tenant h ON h.organization_id=NEW.organization_id
  WHERE m.active=1 AND m.role IN ('owner','admin');

  INSERT INTO token_grants(token_id,organization_id,project_id)
  SELECT m.principal_id,NEW.organization_id,NEW.id
  FROM hosted_members m JOIN hosted_tenant h ON h.organization_id=NEW.organization_id
  WHERE m.active=1 AND m.role IN ('owner','admin')
  ON CONFLICT DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER projects_hosted_grants_delete BEFORE DELETE ON projects
WHEN EXISTS (SELECT 1 FROM hosted_tenant WHERE organization_id=OLD.organization_id)
BEGIN
  DELETE FROM hosted_project_grants WHERE organization_id=OLD.organization_id AND project_id=OLD.id;
  DELETE FROM token_grants WHERE organization_id=OLD.organization_id AND project_id=OLD.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER projects_hosted_grants_delete;
DROP TRIGGER projects_hosted_administrator_grants;
