-- +goose Up
CREATE TABLE daily_summary_preferences (
 user_id TEXT NOT NULL REFERENCES hosted_members(user_id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
 enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
 send_minute INTEGER NOT NULL DEFAULT 360 CHECK(send_minute>=0 AND send_minute<1440 AND send_minute%30=0),
 time_zone TEXT NOT NULL DEFAULT 'UTC',
 weekdays INTEGER NOT NULL DEFAULT 0 CHECK(weekdays IN (0,1)),
 token TEXT NOT NULL UNIQUE,
 window_end INTEGER NOT NULL,
 due_at INTEGER NOT NULL,
 PRIMARY KEY(user_id,organization_id)
);
CREATE INDEX daily_summary_due ON daily_summary_preferences(organization_id,due_at,user_id) WHERE enabled=1;
CREATE TABLE daily_summary_receipts (
 user_id TEXT NOT NULL,
 organization_id TEXT NOT NULL,
 window_end INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('sent','uncertain','rejected','quiet')),
 attempts INTEGER NOT NULL DEFAULT 1,
 recorded_at INTEGER NOT NULL,
 PRIMARY KEY(user_id,organization_id,window_end),
 FOREIGN KEY(user_id,organization_id) REFERENCES daily_summary_preferences(user_id,organization_id) ON DELETE CASCADE
);

CREATE INDEX daily_summary_rework_window ON collaboration_events(organization_id,project_id,work_item_id,analytics_recorded_ns,to_state) WHERE type='workflow.transitioned';

ALTER TABLE hosted_work_views ADD COLUMN completed_from TEXT NOT NULL DEFAULT '';
ALTER TABLE hosted_work_views ADD COLUMN completed_to TEXT NOT NULL DEFAULT '';

-- +goose Down
DROP INDEX daily_summary_rework_window;
ALTER TABLE hosted_work_views DROP COLUMN completed_to;
ALTER TABLE hosted_work_views DROP COLUMN completed_from;
DROP TABLE daily_summary_receipts;
DROP TABLE daily_summary_preferences;
