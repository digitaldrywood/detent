-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_quality_window_seek;
CREATE INDEX CONCURRENTLY change_evidence_quality_window_seek ON change_evidence(organization_id,project_id,quality_at DESC,sequence,change_id) WHERE quality_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_analytics_seek;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_analytics_seek;
CREATE INDEX CONCURRENTLY change_evidence_analytics_seek ON change_evidence(organization_id,project_id,sequence,change_id) WHERE kind IN('review','discussion');
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_quality_window_seek;
RESET lock_timeout;
