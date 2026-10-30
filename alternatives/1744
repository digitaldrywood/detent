-- +goose Up
CREATE INDEX runner_checkout_repositories_project_repository_idx ON runner_checkout_repositories(project_id, repository COLLATE NOCASE);
CREATE INDEX repositories_casefold_name_idx ON repositories(lower(github_owner), lower(github_name));

CREATE TABLE release_deliveries_checkout (
 work_item_id TEXT PRIMARY KEY REFERENCES issues(native_id),
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL REFERENCES projects(id),
 source_issue_id TEXT NOT NULL REFERENCES issues(native_id),
 source_change_id TEXT NOT NULL REFERENCES change_requests(id),
 source_merge_sha TEXT NOT NULL,
 repository_id INTEGER REFERENCES repositories(id),
 repository TEXT NOT NULL COLLATE NOCASE,
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
 UNIQUE (repository_id, pr_number),
 UNIQUE (repository, pr_number)
);
INSERT INTO release_deliveries_checkout SELECT * FROM release_deliveries;
DROP TABLE release_deliveries;
ALTER TABLE release_deliveries_checkout RENAME TO release_deliveries;

-- +goose Down
DROP INDEX runner_checkout_repositories_project_repository_idx;
DROP INDEX repositories_casefold_name_idx;

CREATE TABLE release_deliveries_connector (
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
INSERT INTO release_deliveries_connector SELECT * FROM release_deliveries;
DROP TABLE release_deliveries;
ALTER TABLE release_deliveries_connector RENAME TO release_deliveries;
