-- +goose Up
CREATE INDEX team_threads_unread_scan ON team_threads(organization_id,id);
CREATE TABLE team_thread_unread_summaries (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  principal_id TEXT NOT NULL,
  requested_revision INTEGER NOT NULL DEFAULT 1,
  completed_revision INTEGER NOT NULL DEFAULT 0,
  scan_revision INTEGER NOT NULL DEFAULT 0,
  after_thread TEXT NOT NULL DEFAULT '',
  through_thread TEXT NOT NULL DEFAULT '',
  partial_count INTEGER NOT NULL DEFAULT 0,
  unread_count INTEGER,
  turn INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX team_thread_unread_pending ON team_thread_unread_summaries(turn,id) WHERE requested_revision>completed_revision;
CREATE INDEX team_thread_unread_turn ON team_thread_unread_summaries(turn);
CREATE TABLE team_thread_unread_projects (
  summary_id TEXT NOT NULL REFERENCES team_thread_unread_summaries(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  PRIMARY KEY(summary_id,project_id)
);

-- +goose Down
DROP TABLE team_thread_unread_projects;
DROP TABLE team_thread_unread_summaries;
DROP INDEX team_threads_unread_scan;
