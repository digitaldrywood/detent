-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_analytics_currency_seek;
CREATE INDEX CONCURRENTLY attempt_usage_analytics_currency_seek ON attempt_usage(organization_id,project_id,period,provider,model,attempt_id,currency,usage_origin);
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_latest_total;
CREATE INDEX CONCURRENTLY attempt_usage_latest_total ON attempt_usage(organization_id,attempt_id,provider,model,usage_sequence DESC,period DESC,updated_at DESC,currency COLLATE "C" DESC);
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_latest_currency_total;
CREATE INDEX CONCURRENTLY attempt_usage_latest_currency_total ON attempt_usage(organization_id,attempt_id,provider,model,currency,usage_sequence DESC,period DESC,updated_at DESC);
DROP INDEX CONCURRENTLY IF EXISTS cost_sources_report_refresh_seek;
CREATE INDEX CONCURRENTLY cost_sources_report_refresh_seek ON cost_sources(organization_id,source_key) WHERE bucket='runner_api';
DROP INDEX CONCURRENTLY IF EXISTS report_usage_current_classification;
CREATE INDEX CONCURRENTLY report_usage_current_classification ON report_usage_current(organization_id,attempt_id,provider,model,usage_kind NULLS FIRST,source_key);
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_known_total;
CREATE INDEX CONCURRENTLY attempt_usage_known_total ON attempt_usage(organization_id,attempt_id,provider,model,usage_sequence DESC,period DESC,updated_at DESC,currency COLLATE "C" DESC) WHERE usage_input_total IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_known_currency_total;
CREATE INDEX CONCURRENTLY attempt_usage_known_currency_total ON attempt_usage(organization_id,attempt_id,provider,model,currency,usage_sequence DESC,period DESC,updated_at DESC) WHERE usage_input_total IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_unclassified;
CREATE INDEX CONCURRENTLY attempt_usage_unclassified ON attempt_usage(organization_id,attempt_id,provider,model,period,updated_at,currency) WHERE usage_input_total IS NULL;
DROP INDEX CONCURRENTLY IF EXISTS native_attempt_usage_pending;
CREATE INDEX CONCURRENTLY native_attempt_usage_pending ON native_attempts(organization_id,project_id,id) WHERE usage_pending_sequence>0;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_origin;
CREATE INDEX CONCURRENTLY attempt_usage_origin ON attempt_usage(organization_id,attempt_id,usage_origin,period,provider,model,currency);
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_currency_key;
CREATE UNIQUE INDEX CONCURRENTLY attempt_usage_currency_key ON attempt_usage(organization_id,attempt_id,period,provider,model,currency,usage_origin);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_origin;
DROP INDEX CONCURRENTLY IF EXISTS native_attempt_usage_pending;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_unclassified;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_known_currency_total;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_known_total;
DROP INDEX CONCURRENTLY IF EXISTS report_usage_current_classification;
DROP INDEX CONCURRENTLY IF EXISTS cost_sources_report_refresh_seek;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_latest_currency_total;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_latest_total;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_analytics_currency_seek;
DROP INDEX CONCURRENTLY IF EXISTS attempt_usage_currency_key;
RESET lock_timeout;
