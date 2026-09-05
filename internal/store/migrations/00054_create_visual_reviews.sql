-- +goose Up
CREATE TABLE visual_review_captures (
 project_id TEXT NOT NULL, issue_id TEXT NOT NULL, repository TEXT NOT NULL, pr INTEGER NOT NULL,
 capture_id TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, captured_at TEXT NOT NULL,
 title TEXT NOT NULL, summary TEXT NOT NULL, coverage_notes TEXT NOT NULL, manifest_json BLOB NOT NULL,
 created_at TEXT NOT NULL, PRIMARY KEY(project_id,capture_id)
);
CREATE INDEX visual_review_issue_idx ON visual_review_captures(project_id,issue_id,created_at DESC);
CREATE TABLE visual_review_assets (
 project_id TEXT NOT NULL, capture_id TEXT NOT NULL, asset_id TEXT NOT NULL, storage_key TEXT NOT NULL,
 kind TEXT NOT NULL, media_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, sha256 TEXT NOT NULL,
 width INTEGER NOT NULL, height INTEGER NOT NULL,
 PRIMARY KEY(project_id,capture_id,asset_id), UNIQUE(storage_key),
 FOREIGN KEY(project_id,capture_id) REFERENCES visual_review_captures(project_id,capture_id) ON DELETE CASCADE
);
CREATE TABLE visual_review_drafts (
 project_id TEXT NOT NULL, capture_id TEXT NOT NULL, head_sha TEXT NOT NULL, revision INTEGER NOT NULL,
 feedback_json BLOB NOT NULL, audit_actor TEXT NOT NULL, updated_at TEXT NOT NULL,
 PRIMARY KEY(project_id,capture_id),
 FOREIGN KEY(project_id,capture_id) REFERENCES visual_review_captures(project_id,capture_id) ON DELETE CASCADE
);
-- +goose Down
DROP TABLE IF EXISTS visual_review_drafts;
DROP TABLE IF EXISTS visual_review_assets;
DROP TABLE IF EXISTS visual_review_captures;
