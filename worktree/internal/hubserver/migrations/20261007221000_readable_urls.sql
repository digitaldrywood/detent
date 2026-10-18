-- +goose Up
ALTER TABLE organizations ADD COLUMN slug TEXT NOT NULL DEFAULT '';
CREATE TABLE organization_slugs (slug TEXT NOT NULL, organization_id TEXT NOT NULL REFERENCES organizations(id) ON UPDATE CASCADE ON DELETE CASCADE, PRIMARY KEY (slug));
CREATE UNIQUE INDEX organizations_slug_idx ON organizations(slug) WHERE slug <> '';
-- +goose StatementBegin
CREATE TRIGGER organizations_generate_slug AFTER INSERT ON organizations WHEN NEW.slug = '' BEGIN
 INSERT INTO organization_slugs (slug, organization_id)
 WITH RECURSIVE chars(pos, value) AS (
 SELECT 1, '' UNION ALL
 SELECT pos+1, value || CASE WHEN substr(lower(NEW.name), pos, 1) GLOB '[a-z0-9]' THEN substr(lower(NEW.name), pos, 1) WHEN value = '' OR substr(value, -1) = '-' THEN '' ELSE '-' END FROM chars WHERE pos <= length(NEW.name)
 ), base(value) AS (
 SELECT CASE WHEN trim(substr(value,1,60), '-') = '' THEN 'organization-organization' WHEN trim(substr(value,1,60), '-') IN ('work','settings','login','projects','organizations','organization','api','app','auth','chat','reports','activity','diagnostics','fleet','support','invite','logout','health','metrics','static','webhooks','design-system','setup','usage','billing','account','platform') THEN trim(substr(value,1,60), '-') || '-organization' ELSE trim(substr(value,1,60), '-') END FROM chars ORDER BY pos DESC LIMIT 1
 ), candidates(n, value) AS (
 SELECT 1, value FROM base UNION ALL SELECT n+1, (SELECT value FROM base) || '-' || (n+1) FROM candidates WHERE EXISTS(SELECT 1 FROM organization_slugs WHERE slug = candidates.value)
 ) SELECT value, NEW.id FROM candidates ORDER BY n DESC LIMIT 1;
 UPDATE organizations SET slug = (SELECT slug FROM organization_slugs WHERE organization_id = NEW.id) WHERE id = NEW.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER organizations_backfill_slug AFTER UPDATE OF name ON organizations WHEN NEW.slug = '' BEGIN
 INSERT INTO organization_slugs (slug, organization_id)
 WITH RECURSIVE chars(pos, value) AS (
 SELECT 1, '' UNION ALL
 SELECT pos+1, value || CASE WHEN substr(lower(NEW.name), pos, 1) GLOB '[a-z0-9]' THEN substr(lower(NEW.name), pos, 1) WHEN value = '' OR substr(value, -1) = '-' THEN '' ELSE '-' END FROM chars WHERE pos <= length(NEW.name)
 ), base(value) AS (
 SELECT CASE WHEN trim(substr(value,1,60), '-') = '' THEN 'organization-organization' WHEN trim(substr(value,1,60), '-') IN ('work','settings','login','projects','organizations','organization','api','app','auth','chat','reports','activity','diagnostics','fleet','support','invite','logout','health','metrics','static','webhooks','design-system','setup','usage','billing','account','platform') THEN trim(substr(value,1,60), '-') || '-organization' ELSE trim(substr(value,1,60), '-') END FROM chars ORDER BY pos DESC LIMIT 1
 ), candidates(n, value) AS (
 SELECT 1, value FROM base UNION ALL SELECT n+1, (SELECT value FROM base) || '-' || (n+1) FROM candidates WHERE EXISTS(SELECT 1 FROM organization_slugs WHERE slug = candidates.value)
 ) SELECT value, NEW.id FROM candidates ORDER BY n DESC LIMIT 1;
 UPDATE organizations SET slug = (SELECT slug FROM organization_slugs WHERE organization_id = NEW.id) WHERE id = NEW.id;
END;
-- +goose StatementEnd
UPDATE organizations SET name = name;
DROP TRIGGER organizations_backfill_slug;
ALTER TABLE projects ADD COLUMN slug TEXT NOT NULL DEFAULT '';
CREATE TABLE project_slugs (organization_id TEXT NOT NULL REFERENCES organizations(id) ON UPDATE CASCADE ON DELETE CASCADE, slug TEXT NOT NULL, project_id TEXT NOT NULL REFERENCES projects(id) ON UPDATE CASCADE ON DELETE CASCADE, PRIMARY KEY (organization_id, slug));
CREATE UNIQUE INDEX projects_slug_idx ON projects(organization_id, slug) WHERE slug <> '';
-- +goose StatementBegin
CREATE TRIGGER projects_generate_slug AFTER INSERT ON projects WHEN NEW.slug = '' BEGIN
 INSERT INTO project_slugs (organization_id, slug, project_id)
 WITH RECURSIVE chars(pos, value) AS (
 SELECT 1, '' UNION ALL
 SELECT pos+1, value || CASE WHEN substr(lower(NEW.name), pos, 1) GLOB '[a-z0-9]' THEN substr(lower(NEW.name), pos, 1) WHEN value = '' OR substr(value, -1) = '-' THEN '' ELSE '-' END FROM chars WHERE pos <= length(NEW.name)
 ), base(value) AS (
 SELECT CASE WHEN trim(substr(value,1,60), '-') = '' THEN 'project' WHEN trim(substr(value,1,60), '-') IN ('work','settings','login','projects','organizations','organization','api','app','auth','chat','reports','activity','diagnostics','fleet','support','invite','logout','health','metrics','static','webhooks','design-system','setup','usage','billing','account','platform') THEN trim(substr(value,1,60), '-') || '-project' ELSE trim(substr(value,1,60), '-') END FROM chars ORDER BY pos DESC LIMIT 1
 ), candidates(n, value) AS (
 SELECT 1, value FROM base UNION ALL SELECT n+1, (SELECT value FROM base) || '-' || (n+1) FROM candidates WHERE EXISTS(SELECT 1 FROM project_slugs WHERE organization_id = NEW.organization_id AND slug = candidates.value)
 ) SELECT NEW.organization_id, value, NEW.id FROM candidates ORDER BY n DESC LIMIT 1;
 UPDATE projects SET slug = (SELECT slug FROM project_slugs WHERE project_id = NEW.id) WHERE id = NEW.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER projects_backfill_slug AFTER UPDATE OF name ON projects WHEN NEW.slug = '' BEGIN
 INSERT INTO project_slugs (organization_id, slug, project_id)
 WITH RECURSIVE chars(pos, value) AS (
 SELECT 1, '' UNION ALL
 SELECT pos+1, value || CASE WHEN substr(lower(NEW.name), pos, 1) GLOB '[a-z0-9]' THEN substr(lower(NEW.name), pos, 1) WHEN value = '' OR substr(value, -1) = '-' THEN '' ELSE '-' END FROM chars WHERE pos <= length(NEW.name)
 ), base(value) AS (
 SELECT CASE WHEN trim(substr(value,1,60), '-') = '' THEN 'project' WHEN trim(substr(value,1,60), '-') IN ('work','settings','login','projects','organizations','organization','api','app','auth','chat','reports','activity','diagnostics','fleet','support','invite','logout','health','metrics','static','webhooks','design-system','setup','usage','billing','account','platform') THEN trim(substr(value,1,60), '-') || '-project' ELSE trim(substr(value,1,60), '-') END FROM chars ORDER BY pos DESC LIMIT 1
 ), candidates(n, value) AS (
 SELECT 1, value FROM base UNION ALL SELECT n+1, (SELECT value FROM base) || '-' || (n+1) FROM candidates WHERE EXISTS(SELECT 1 FROM project_slugs WHERE organization_id = NEW.organization_id AND slug = candidates.value)
 ) SELECT NEW.organization_id, value, NEW.id FROM candidates ORDER BY n DESC LIMIT 1;
 UPDATE projects SET slug = (SELECT slug FROM project_slugs WHERE project_id = NEW.id) WHERE id = NEW.id;
END;
-- +goose StatementEnd
UPDATE projects SET name = name;
DROP TRIGGER projects_backfill_slug;

-- +goose Down
SELECT 1;
