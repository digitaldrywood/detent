-- +goose Up
SET LOCAL lock_timeout = '2s';
ALTER TABLE machines ADD COLUMN registration_token_id text;
ALTER TABLE machines ADD CONSTRAINT machines_registration_token_fk FOREIGN KEY (organization_id, registration_token_id) REFERENCES api_tokens (organization_id, id) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '2s';
ALTER TABLE machines DROP COLUMN registration_token_id;
