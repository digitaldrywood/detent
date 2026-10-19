-- +goose Up
CREATE TABLE local_board_imports (
 organization_id TEXT NOT NULL REFERENCES organizations(id),
 project_id TEXT NOT NULL REFERENCES projects(id),
 source_key TEXT NOT NULL,
 work_item_id TEXT NOT NULL REFERENCES issues(native_id),
 created_at TEXT NOT NULL,
 PRIMARY KEY (organization_id, project_id, source_key)
);

-- +goose Down
DROP TABLE local_board_imports;
