-- +goose Up
CREATE TABLE health_findings (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  fingerprint TEXT NOT NULL,
  signal TEXT NOT NULL,
  class TEXT NOT NULL CHECK (class IN ('instance', 'flow', 'capacity', 'cost', 'human')),
  subject_json TEXT NOT NULL CHECK (json_valid(subject_json)),
  projects_json TEXT NOT NULL CHECK (json_valid(projects_json)),
  opened_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  resolved_at TEXT,
  severity TEXT NOT NULL CHECK (severity IN ('attention', 'watch')),
  summary TEXT NOT NULL,
  next_action TEXT NOT NULL,
  evidence_json TEXT NOT NULL CHECK (json_valid(evidence_json))
);
CREATE UNIQUE INDEX health_findings_open ON health_findings(organization_id, fingerprint) WHERE resolved_at IS NULL;
CREATE INDEX health_findings_recent ON health_findings(organization_id, fingerprint, resolved_at DESC);
CREATE INDEX health_findings_page ON health_findings(organization_id, last_seen_at DESC, id);
CREATE TABLE health_detector_ticks (
  organization_id TEXT PRIMARY KEY REFERENCES organizations(id),
  last_tick_at TEXT NOT NULL
);
CREATE INDEX health_decision_window ON collaboration_events(organization_id, julianday(recorded_at), id) WHERE type='scheduler.decision';
CREATE INDEX health_landing_window ON collaboration_events(organization_id, julianday(recorded_at), id) WHERE type IN ('run.finished','run.observed') AND json_extract(data_json,'$.run.runtime.landing.landed')=1;
CREATE INDEX health_active_host ON leases(machine_id,expires_at,lease_id) WHERE released_at IS NULL;
CREATE INDEX health_event_current ON collaboration_events(organization_id, work_item_id, type, sequence DESC);
CREATE INDEX health_attempt_current ON native_attempts(organization_id, project_id, work_item_id, fencing_token DESC);

-- +goose Down
DROP INDEX health_attempt_current;
DROP INDEX health_event_current;
DROP INDEX health_active_host;
DROP INDEX health_decision_window;
DROP INDEX health_landing_window;
DROP TABLE health_detector_ticks;
DROP TABLE health_findings;
