-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_evidence_analytics_seek;
CREATE INDEX CONCURRENTLY change_evidence_analytics_seek ON change_evidence (organization_id,project_id,sequence,change_id) WHERE kind IN('review','discussion');
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_analytics_workflow_seek;
CREATE INDEX CONCURRENTLY collaboration_events_analytics_workflow_seek ON collaboration_events (organization_id,project_id,recorded_at,sequence,id) WHERE type='workflow.transitioned';
DROP INDEX CONCURRENTLY IF EXISTS change_requests_analytics_landed_seek;
CREATE INDEX CONCURRENTLY change_requests_analytics_landed_seek ON change_requests (organization_id,project_id,landed_at,id) WHERE landed_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS change_versions_analytics_landed_seek;
CREATE INDEX CONCURRENTLY change_versions_analytics_landed_seek ON change_versions (organization_id,project_id,landed_at,change_id,id) WHERE landed_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS change_versions_unknown_landing_history;
CREATE INDEX CONCURRENTLY change_versions_unknown_landing_history ON change_versions (organization_id,project_id,id) WHERE NOT landing_history_known;
DROP INDEX CONCURRENTLY IF EXISTS report_escape_facts_occurrence_seek;
CREATE INDEX CONCURRENTLY report_escape_facts_occurrence_seek ON report_escape_facts(organization_id,change_id,occurrence_id,evidence_sequence DESC);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY report_escape_facts_occurrence_seek;
DROP INDEX CONCURRENTLY change_versions_unknown_landing_history;
DROP INDEX CONCURRENTLY change_versions_analytics_landed_seek;
DROP INDEX CONCURRENTLY change_requests_analytics_landed_seek;
DROP INDEX CONCURRENTLY collaboration_events_analytics_workflow_seek;
DROP INDEX CONCURRENTLY change_evidence_analytics_seek;
RESET lock_timeout;
