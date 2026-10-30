-- +goose Up
CREATE TABLE hosted_work_view_projects (
 view_id TEXT NOT NULL REFERENCES hosted_work_views(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 position INTEGER NOT NULL CHECK(position >= 0 AND position < 100),
 PRIMARY KEY(view_id, project_id),
 UNIQUE(view_id, position)
);
INSERT INTO hosted_work_view_projects(view_id, project_id, position)
SELECT id, project_id, 0 FROM hosted_work_views WHERE project_id IS NOT NULL;

-- +goose Down
DROP TABLE hosted_work_view_projects;
