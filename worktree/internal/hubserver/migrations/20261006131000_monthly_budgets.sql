-- +goose Up
CREATE TABLE monthly_budget_policies (
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  project_id TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL CHECK (revision > 0),
  policy_json TEXT NOT NULL CHECK (json_valid(policy_json)),
  actor_id TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, revision)
);
CREATE TABLE monthly_budget_admissions (
  issue_id INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
  admitted_at TEXT NOT NULL
);
CREATE TABLE monthly_budget_leases (
  lease_id TEXT PRIMARY KEY REFERENCES leases(lease_id) ON DELETE CASCADE,
  exposure_json TEXT NOT NULL CHECK (json_valid(exposure_json))
);
INSERT INTO monthly_budget_admissions(issue_id, admitted_at)
SELECT i.id, min(l.acquired_at) FROM issues i JOIN leases l ON l.issue_id=i.id
JOIN projects p ON p.id=i.project_id AND p.profile='native'
JOIN workflow_states w ON w.id=i.workflow_state_id
WHERE w.terminal=0 AND lower(w.detent_state) NOT IN ('blocked','backlog','cancelled') AND i.archived=0
GROUP BY i.id;

-- +goose StatementBegin
CREATE TRIGGER monthly_budget_terminal_cleanup AFTER UPDATE OF workflow_state_id,archived ON issues
WHEN NEW.archived=1 OR EXISTS (SELECT 1 FROM workflow_states w WHERE w.id=NEW.workflow_state_id AND (w.terminal=1 OR lower(w.detent_state) IN ('blocked','backlog','cancelled')))
BEGIN
 DELETE FROM monthly_budget_admissions WHERE issue_id=NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER monthly_budget_terminal_cleanup;
DROP TABLE monthly_budget_leases;
DROP TABLE monthly_budget_admissions;
DROP TABLE monthly_budget_policies;
