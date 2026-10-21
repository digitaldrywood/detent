-- +goose Up
CREATE TABLE report_issue_summaries (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 created_ns INTEGER NOT NULL,
 closed_ns INTEGER NOT NULL,
 state TEXT NOT NULL,
 issue_type TEXT NOT NULL,
 priority TEXT NOT NULL,
 filer_id TEXT NOT NULL,
 filer_kind TEXT NOT NULL,
 completed_kind TEXT NOT NULL,
 completed_id TEXT NOT NULL,
 completed_name TEXT NOT NULL,
 cycle_seconds REAL,
 PRIMARY KEY(organization_id,project_id,work_item_id),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id) ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE INDEX report_issue_created ON report_issue_summaries(organization_id,project_id,created_ns);
CREATE INDEX report_issue_closed ON report_issue_summaries(organization_id,project_id,closed_ns);
CREATE TABLE report_issue_labels (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 label TEXT NOT NULL,
 PRIMARY KEY(organization_id,project_id,work_item_id,label),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES report_issue_summaries ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE TABLE report_issue_hours (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 hour INTEGER NOT NULL,
 cost_usd REAL NOT NULL,
 worked INTEGER NOT NULL,
 reworked INTEGER NOT NULL,
 landed INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id,work_item_id,hour),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES report_issue_summaries ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE TABLE report_issue_runners (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 runner_id TEXT NOT NULL,
 started_ns INTEGER NOT NULL,
 updated_ns INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id,work_item_id,runner_id,started_ns),
 FOREIGN KEY(organization_id,project_id,work_item_id) REFERENCES report_issue_summaries ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO analytics_rollup_backfill SELECT organization_id,id FROM projects WHERE true ON CONFLICT DO NOTHING;
DELETE FROM analytics_rollup_positions;

-- +goose Down
DROP TABLE report_issue_runners;
DROP TABLE report_issue_hours;
DROP TABLE report_issue_labels;
DROP TABLE report_issue_summaries;
