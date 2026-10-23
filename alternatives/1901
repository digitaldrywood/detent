-- +goose Up
CREATE TABLE health_finding_issues_signatures (
  finding_id TEXT NOT NULL REFERENCES health_findings(id),
  project_id TEXT NOT NULL REFERENCES projects(id),
  work_item_id TEXT NOT NULL REFERENCES issues(native_id),
  intake_fingerprint TEXT NOT NULL DEFAULT '',
  reported_evidence_digest TEXT NOT NULL DEFAULT '',
  reported_resolved_at TEXT,
  PRIMARY KEY (finding_id, project_id, intake_fingerprint)
);

INSERT INTO health_finding_issues_signatures(finding_id,project_id,work_item_id,reported_resolved_at)
SELECT finding_id,project_id,work_item_id,reported_resolved_at FROM health_finding_issues;

DROP TABLE health_finding_issues;
ALTER TABLE health_finding_issues_signatures RENAME TO health_finding_issues;
CREATE INDEX health_finding_issues_item ON health_finding_issues(work_item_id);

-- +goose Down
CREATE TABLE health_finding_issues_legacy (
  finding_id TEXT NOT NULL REFERENCES health_findings(id),
  project_id TEXT NOT NULL REFERENCES projects(id),
  work_item_id TEXT NOT NULL REFERENCES issues(native_id),
  reported_evidence_json TEXT NOT NULL CHECK (json_valid(reported_evidence_json)),
  reported_resolved_at TEXT,
  PRIMARY KEY (finding_id, project_id)
);

INSERT INTO health_finding_issues_legacy(finding_id,project_id,work_item_id,reported_evidence_json,reported_resolved_at)
SELECT l.finding_id,l.project_id,l.work_item_id,f.evidence_json,l.reported_resolved_at
FROM health_finding_issues l JOIN health_findings f ON f.id=l.finding_id
WHERE l.rowid IN (SELECT min(rowid) FROM health_finding_issues GROUP BY finding_id,project_id);

DROP TABLE health_finding_issues;
ALTER TABLE health_finding_issues_legacy RENAME TO health_finding_issues;
