-- +goose Up
CREATE TABLE release_deliveries (
 work_item_id TEXT PRIMARY KEY REFERENCES issues(native_id),
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL REFERENCES projects(id),
 source_issue_id TEXT NOT NULL REFERENCES issues(native_id),
 source_change_id TEXT NOT NULL REFERENCES change_requests(id),
 source_merge_sha TEXT NOT NULL,
 repository_id INTEGER NOT NULL REFERENCES repositories(id),
 repository TEXT NOT NULL,
 pr_number INTEGER NOT NULL CHECK (pr_number > 0),
 head_ref TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 base_ref TEXT NOT NULL,
 base_sha TEXT NOT NULL,
 frozen_sha TEXT NOT NULL,
 change_id TEXT NOT NULL REFERENCES change_requests(id),
 version_id TEXT REFERENCES change_versions(id),
 principal_id TEXT NOT NULL,
 actor_kind TEXT NOT NULL,
 via_key TEXT NOT NULL,
 associated_at TEXT NOT NULL,
 UNIQUE (repository_id, pr_number)
);

-- +goose Down
DROP TABLE release_deliveries;
