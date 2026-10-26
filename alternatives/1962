-- +goose Up
CREATE INDEX analytics_activity_issue_edges ON analytics_hourly_rollups(organization_id,project_id,substr(dimension,1,instr(dimension,':')-1),hour DESC,value DESC) WHERE metric='residence_edge';
CREATE INDEX native_attempts_activity_issue ON native_attempts(organization_id,project_id,work_item_id,started_at DESC,id DESC);
CREATE INDEX issues_activity_terminal ON issues(organization_id,project_id,terminal_entered_at DESC,native_id);

-- +goose Down
DROP INDEX issues_activity_terminal;
DROP INDEX native_attempts_activity_issue;
DROP INDEX analytics_activity_issue_edges;
