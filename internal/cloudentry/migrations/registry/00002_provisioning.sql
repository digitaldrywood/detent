-- +goose Up
DROP TRIGGER organization_identity_immutable;
DROP TRIGGER organization_delete;

CREATE TABLE organizations_next (
  id TEXT PRIMARY KEY CHECK (id GLOB 'org_*' AND length(id) <= 64),
  provider_id TEXT NOT NULL DEFAULT '' CHECK (length(provider_id) <= 128),
  name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
  state TEXT NOT NULL CHECK (state IN ('requested', 'allocating', 'ready', 'failed', 'disabled', 'deleting', 'deleted')),
  endpoint TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK (generation >= 1),
  managed INTEGER NOT NULL DEFAULT 0 CHECK (managed IN (0, 1)),
  creator_subject TEXT NOT NULL DEFAULT '',
  creator_email TEXT NOT NULL DEFAULT '',
  step TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO organizations_next(id,provider_id,name,state,endpoint,generation,created_at,updated_at)
  SELECT id,provider_id,name,state,endpoint,generation,created_at,updated_at FROM organizations;
DROP TABLE organizations;
ALTER TABLE organizations_next RENAME TO organizations;
CREATE UNIQUE INDEX organizations_provider ON organizations(provider_id) WHERE provider_id != '';
CREATE INDEX organizations_creator ON organizations(creator_subject, state);

CREATE TABLE organization_intents (
  subject TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  organization_id TEXT NOT NULL UNIQUE REFERENCES organizations(id),
  created_at TEXT NOT NULL,
  PRIMARY KEY (subject, idempotency_key)
);

-- +goose StatementBegin
CREATE TRIGGER organization_identity_immutable BEFORE UPDATE ON organizations
WHEN NEW.id IS NOT OLD.id
  OR (OLD.provider_id != '' AND NEW.provider_id IS NOT OLD.provider_id)
  OR NEW.generation < OLD.generation
  OR NEW.creator_subject IS NOT OLD.creator_subject
  OR (OLD.state = 'deleted' AND NEW.state IS NOT 'deleted')
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
