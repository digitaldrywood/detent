-- +goose Up
CREATE TABLE health_event_times (
 event_id TEXT PRIMARY KEY REFERENCES collaboration_events(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL REFERENCES organizations(id),
 recorded_at_ns INTEGER NOT NULL,
 type TEXT NOT NULL,
 landed INTEGER NOT NULL CHECK (landed IN (0,1))
);
CREATE INDEX health_decision_time ON health_event_times(organization_id,recorded_at_ns,event_id) WHERE type='scheduler.decision';
CREATE INDEX health_landing_time ON health_event_times(organization_id,recorded_at_ns,event_id) WHERE type IN ('run.finished','run.observed') AND landed=1;
ALTER TABLE hosted_audit ADD COLUMN recorded_at_ns INTEGER;
CREATE TABLE hosted_audit_times (
 audit_id INTEGER PRIMARY KEY REFERENCES hosted_audit(id) ON DELETE CASCADE,
 recorded_at_ns INTEGER NOT NULL
);
CREATE INDEX hosted_audit_time ON hosted_audit_times(recorded_at_ns,audit_id);
-- +goose StatementBegin
CREATE TRIGGER hosted_audit_time_insert AFTER INSERT ON hosted_audit WHEN NEW.recorded_at_ns IS NOT NULL BEGIN
 INSERT INTO hosted_audit_times(audit_id,recorded_at_ns) VALUES(NEW.id,NEW.recorded_at_ns);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_audit_time_update AFTER UPDATE OF recorded_at_ns ON hosted_audit WHEN NEW.recorded_at_ns IS NOT NULL BEGIN
 INSERT INTO hosted_audit_times(audit_id,recorded_at_ns) VALUES(NEW.id,NEW.recorded_at_ns) ON CONFLICT(audit_id) DO UPDATE SET recorded_at_ns=excluded.recorded_at_ns;
END;
-- +goose StatementEnd
CREATE TABLE time_index_progress (
 owner TEXT PRIMARY KEY,
 last_rowid INTEGER NOT NULL,
 complete INTEGER NOT NULL CHECK (complete IN (0,1))
);

-- +goose Down
DROP TABLE time_index_progress;
DROP TRIGGER hosted_audit_time_update;
DROP TRIGGER hosted_audit_time_insert;
DROP TABLE hosted_audit_times;
ALTER TABLE hosted_audit DROP COLUMN recorded_at_ns;
DROP TABLE health_event_times;
