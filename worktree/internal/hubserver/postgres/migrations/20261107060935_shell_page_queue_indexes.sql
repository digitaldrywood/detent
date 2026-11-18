-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS project_integration_page_queue;
CREATE INDEX CONCURRENTLY project_integration_page_queue ON project_integration_page_dirty(organization_id,attempted_at,project_id);
DROP INDEX CONCURRENTLY IF EXISTS project_onboarding_page_next;
CREATE INDEX CONCURRENTLY project_onboarding_page_next ON project_onboarding_page_viewers(organization_id,attempted_at,project_id,viewer_key);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS project_integration_page_queue;
CREATE INDEX CONCURRENTLY project_integration_page_queue ON project_integration_page_dirty(organization_id,attempted_at,project_id);
DROP INDEX CONCURRENTLY IF EXISTS project_onboarding_page_next;
CREATE INDEX CONCURRENTLY project_onboarding_page_next ON project_onboarding_page_viewers(organization_id,attempted_at,project_id,viewer_key);
