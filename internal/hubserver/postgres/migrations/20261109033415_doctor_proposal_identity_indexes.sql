-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS doctor_proposal_unknown_heads;
CREATE INDEX CONCURRENTLY doctor_proposal_unknown_heads ON issues(organization_id,project_id,native_id) WHERE NOT archived AND doctor_identity_revision IS DISTINCT FROM revision;
DROP INDEX CONCURRENTLY IF EXISTS doctor_proposal_identity_state;
CREATE INDEX CONCURRENTLY doctor_proposal_identity_state ON doctor_proposal_identities(organization_id,project_id,kind,identity_hash,has_state,state_name,item_number DESC,work_item_id) INCLUDE(source_revision,version_sha256) WHERE NOT archived AND kind<>'complete';
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS doctor_proposal_identity_state;
DROP INDEX CONCURRENTLY IF EXISTS doctor_proposal_unknown_heads;
