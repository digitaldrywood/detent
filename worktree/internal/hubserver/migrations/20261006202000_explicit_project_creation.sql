-- +goose Up
DROP TRIGGER repositories_project_alias;

-- +goose Down
-- +goose StatementBegin
CREATE TRIGGER repositories_project_alias AFTER INSERT ON repositories
BEGIN
  INSERT INTO projects (organization_id, repository_id, name, profile, created_at)
  SELECT id, NEW.id, NEW.github_owner || '/' || NEW.github_name, 'github_compatible', NEW.created_at FROM organizations WHERE local = 1;
END;
-- +goose StatementEnd
