-- +goose Up
ALTER TABLE hosted_tenant ADD COLUMN deployment TEXT NOT NULL DEFAULT 'origin' CHECK (deployment IN ('origin', 'shared'));
ALTER TABLE hosted_tenant ADD COLUMN allocation_generation INTEGER NOT NULL DEFAULT 0 CHECK (allocation_generation >= 0);

CREATE TABLE hosted_binding_migrations (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  from_deployment TEXT NOT NULL,
  from_public_url TEXT NOT NULL,
  from_generation INTEGER NOT NULL,
  to_public_url TEXT NOT NULL,
  to_generation INTEGER NOT NULL CHECK (to_generation > from_generation),
  migrated_at TEXT NOT NULL,
  applied INTEGER NOT NULL DEFAULT 0 CHECK (applied IN (0, 1))
);

DROP TRIGGER hosted_tenant_binding_update;

-- +goose StatementBegin
CREATE TRIGGER hosted_tenant_binding_update BEFORE UPDATE ON hosted_tenant
WHEN NEW.singleton IS NOT OLD.singleton
  OR NEW.organization_id IS NOT OLD.organization_id
  OR NEW.bootstrap_subject IS NOT OLD.bootstrap_subject
  OR (OLD.provider_id != '' AND NEW.provider_id IS NOT OLD.provider_id)
  OR ((NEW.public_url IS NOT OLD.public_url OR NEW.deployment IS NOT OLD.deployment OR NEW.allocation_generation IS NOT OLD.allocation_generation)
    AND NOT EXISTS (
      SELECT 1 FROM hosted_binding_migrations m
      WHERE m.organization_id = OLD.organization_id AND m.applied = 0 AND NEW.deployment = 'shared'
        AND m.from_deployment = OLD.deployment AND m.from_public_url = OLD.public_url AND m.from_generation = OLD.allocation_generation
        AND m.to_public_url = NEW.public_url AND m.to_generation = NEW.allocation_generation
    ))
BEGIN
  SELECT RAISE(ABORT, 'hosted organization binding is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER hosted_binding_migration_update BEFORE UPDATE ON hosted_binding_migrations
WHEN NEW.id IS NOT OLD.id OR NEW.organization_id IS NOT OLD.organization_id OR NEW.from_deployment IS NOT OLD.from_deployment
  OR NEW.from_public_url IS NOT OLD.from_public_url OR NEW.from_generation IS NOT OLD.from_generation
  OR NEW.to_public_url IS NOT OLD.to_public_url OR NEW.to_generation IS NOT OLD.to_generation
  OR NEW.migrated_at IS NOT OLD.migrated_at OR OLD.applied != 0 OR NEW.applied != 1
BEGIN
  SELECT RAISE(ABORT, 'hosted binding migration history is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER hosted_binding_migration_delete BEFORE DELETE ON hosted_binding_migrations
BEGIN
  SELECT RAISE(ABORT, 'hosted binding migration history is immutable');
END;
-- +goose StatementEnd
