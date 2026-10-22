-- +goose Up
CREATE TABLE analytics_sample_hours (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 hour INTEGER NOT NULL,
 metric TEXT NOT NULL,
 dimension TEXT NOT NULL,
 bucket INTEGER NOT NULL,
 count INTEGER NOT NULL,
 total REAL NOT NULL,
 minimum REAL NOT NULL,
 maximum REAL NOT NULL,
 unavailable INTEGER NOT NULL,
 last_ns INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id,metric,hour,dimension,bucket),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE TABLE analytics_residence_retention (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 through_hour INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id,work_item_id),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE TABLE analytics_slot_retention (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 through_hour INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
INSERT INTO analytics_rollup_dirty(work_item_id)
SELECT DISTINCT json_work_item_id FROM analytics_hourly_rollups WHERE metric='attempt' AND json_work_item_id IS NOT NULL
ON CONFLICT(work_item_id) DO NOTHING;

-- +goose Down
DROP TABLE analytics_slot_retention;
DROP TABLE analytics_residence_retention;
DROP TABLE analytics_sample_hours;
