-- +goose Up
CREATE INDEX hosted_invitations_pending_email ON hosted_invitations(email, organization_id) WHERE accepted_user_id = '';

-- +goose Down
DROP INDEX hosted_invitations_pending_email;
