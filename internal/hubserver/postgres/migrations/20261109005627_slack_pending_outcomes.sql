-- +goose Up
ALTER TABLE slack_integrations ADD COLUMN disabled_revision bigint NOT NULL DEFAULT 0 CHECK (disabled_revision>=0);
ALTER TABLE health_slack_deliveries ADD COLUMN integration_revision bigint NOT NULL DEFAULT 1 CHECK (integration_revision>0);
DROP INDEX slack_delivery_pending;
CREATE INDEX slack_delivery_pending ON health_slack_deliveries(organization_id,next_attempt_at,id) WHERE completed_at IS NULL AND outcome<>'uncertain';

-- +goose Down
ALTER TABLE health_slack_deliveries DROP COLUMN integration_revision;
ALTER TABLE slack_integrations DROP COLUMN disabled_revision;
DROP INDEX slack_delivery_pending;
CREATE INDEX slack_delivery_pending ON health_slack_deliveries(organization_id,next_attempt_at,id) WHERE completed_at IS NULL;
