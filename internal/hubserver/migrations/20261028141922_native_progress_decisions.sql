-- +goose Up
ALTER TABLE native_comments ADD COLUMN progress_request_id TEXT NOT NULL DEFAULT '';
CREATE INDEX native_comments_progress_input ON native_comments(organization_id, project_id, work_item_id, sequence DESC) WHERE progress_request_id = '' AND deleted = 0;
CREATE INDEX native_attempts_progress_success ON native_attempts(organization_id, project_id, work_item_id, fencing_token DESC) WHERE status = 'succeeded';
CREATE INDEX native_progress_human_input ON collaboration_events(organization_id, project_id, work_item_id, sequence DESC) WHERE actor_kind = 'human' AND type IN ('comment.created', 'comment.edited');
CREATE TABLE native_progress_decisions (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  evidence_revision TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('progressing', 'reroute', 'system_defect')),
  step TEXT NOT NULL,
  reason TEXT NOT NULL,
  defect_key TEXT NOT NULL,
  defect_title TEXT NOT NULL,
  defect_body TEXT NOT NULL,
  comment_id TEXT NOT NULL REFERENCES native_comments(id),
  defect_issue_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, work_item_id, request_id),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
CREATE TABLE native_progress_evidence (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  position INTEGER NOT NULL,
  evidence_id TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, work_item_id, request_id, position),
  FOREIGN KEY (organization_id, project_id, work_item_id, request_id)
    REFERENCES native_progress_decisions(organization_id, project_id, work_item_id, request_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX native_progress_comment ON native_progress_decisions(comment_id);
CREATE INDEX native_progress_latest_defect ON native_progress_decisions(organization_id, project_id, work_item_id, outcome, created_at DESC, request_id DESC);

-- +goose Down
DROP TABLE native_progress_evidence;
DROP TABLE native_progress_decisions;
DROP INDEX native_comments_progress_input;
DROP INDEX native_attempts_progress_success;
DROP INDEX native_progress_human_input;
ALTER TABLE native_comments DROP COLUMN progress_request_id;
