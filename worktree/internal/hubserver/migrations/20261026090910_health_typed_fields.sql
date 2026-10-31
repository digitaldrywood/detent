-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN health_projected INTEGER;
ALTER TABLE collaboration_events ADD COLUMN scheduler_present INTEGER;
ALTER TABLE collaboration_events ADD COLUMN scheduler_at TEXT;
ALTER TABLE collaboration_events ADD COLUMN scheduler_revision INTEGER;
ALTER TABLE collaboration_events ADD COLUMN transition_from TEXT;
ALTER TABLE collaboration_events ADD COLUMN transition_to TEXT;
ALTER TABLE collaboration_events ADD COLUMN transition_reason TEXT;
ALTER TABLE collaboration_events ADD COLUMN transition_detail TEXT;
ALTER TABLE collaboration_events ADD COLUMN transition_revision INTEGER;
ALTER TABLE collaboration_events ADD COLUMN event_landed INTEGER;
ALTER TABLE collaboration_events ADD COLUMN event_landing_at TEXT;
ALTER TABLE native_comments ADD COLUMN actor_kind TEXT;
ALTER TABLE scheduled_validation_reports ADD COLUMN conclusion TEXT;
ALTER TABLE health_findings ADD COLUMN projects_projected INTEGER;
ALTER TABLE native_attempts ADD COLUMN health_disposition_reason TEXT;
DROP INDEX IF EXISTS health_landing_window;
CREATE INDEX collaboration_events_health_conversion_idx ON collaboration_events(health_projected) WHERE health_projected IS NULL;
CREATE INDEX issues_health_conversion_idx ON issues(actor_kind) WHERE actor_kind IS NULL;
CREATE INDEX native_comments_health_conversion_idx ON native_comments(actor_kind) WHERE actor_kind IS NULL;
CREATE INDEX scheduled_validation_reports_health_conversion_idx ON scheduled_validation_reports(conclusion) WHERE conclusion IS NULL;
CREATE INDEX health_findings_health_conversion_idx ON health_findings(projects_projected) WHERE projects_projected IS NULL;
CREATE INDEX native_attempts_health_conversion_idx ON native_attempts(health_disposition_reason) WHERE health_disposition_reason IS NULL;


-- +goose Down
ALTER TABLE collaboration_events DROP COLUMN scheduler_present;
ALTER TABLE collaboration_events DROP COLUMN scheduler_at;
ALTER TABLE collaboration_events DROP COLUMN scheduler_revision;
DROP INDEX IF EXISTS health_landing_window;
DROP INDEX native_attempts_health_conversion_idx;
DROP INDEX health_findings_health_conversion_idx;
DROP INDEX scheduled_validation_reports_health_conversion_idx;
DROP INDEX native_comments_health_conversion_idx;
DROP INDEX issues_health_conversion_idx;
DROP INDEX collaboration_events_health_conversion_idx;
ALTER TABLE native_attempts DROP COLUMN health_disposition_reason;
ALTER TABLE health_findings DROP COLUMN projects_projected;
ALTER TABLE scheduled_validation_reports DROP COLUMN conclusion;
ALTER TABLE native_comments DROP COLUMN actor_kind;
ALTER TABLE collaboration_events DROP COLUMN event_landing_at;
ALTER TABLE collaboration_events DROP COLUMN event_landed;
ALTER TABLE collaboration_events DROP COLUMN transition_revision;
ALTER TABLE collaboration_events DROP COLUMN transition_detail;
ALTER TABLE collaboration_events DROP COLUMN transition_reason;
ALTER TABLE collaboration_events DROP COLUMN transition_to;
ALTER TABLE collaboration_events DROP COLUMN transition_from;
ALTER TABLE collaboration_events DROP COLUMN health_projected;
