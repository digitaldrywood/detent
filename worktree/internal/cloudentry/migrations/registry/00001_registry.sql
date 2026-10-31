-- +goose Up
CREATE TABLE organizations (
  id TEXT PRIMARY KEY CHECK (id GLOB 'org_*' AND length(id) <= 64),
  provider_id TEXT NOT NULL UNIQUE CHECK (length(provider_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
  state TEXT NOT NULL CHECK (state IN ('ready', 'disabled')),
  endpoint TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK (generation >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE organization_events (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  event TEXT NOT NULL,
  generation INTEGER NOT NULL,
  recorded_at TEXT NOT NULL
);

-- +goose StatementBegin
CREATE TRIGGER organization_identity_immutable BEFORE UPDATE ON organizations
WHEN NEW.id IS NOT OLD.id OR NEW.provider_id IS NOT OLD.provider_id OR NEW.generation < OLD.generation
BEGIN
  SELECT RAISE(ABORT, 'organization identity is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER organization_delete BEFORE DELETE ON organizations
BEGIN
  SELECT RAISE(ABORT, 'organization records are retained');
END;
-- +goose StatementEnd
