-- +goose Up
CREATE TABLE health_finding_issues (
  finding_id TEXT NOT NULL REFERENCES health_findings(id),
  project_id TEXT NOT NULL REFERENCES projects(id),
  work_item_id TEXT NOT NULL REFERENCES issues(native_id),
  reported_evidence_json TEXT NOT NULL CHECK (json_valid(reported_evidence_json)),
  reported_resolved_at TEXT,
  PRIMARY KEY (finding_id, project_id)
);

-- +goose Down
DROP TABLE health_finding_issues;
