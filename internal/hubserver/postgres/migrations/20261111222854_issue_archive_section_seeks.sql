-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issue_archive_sections_range;
CREATE INDEX CONCURRENTLY issue_archive_sections_range ON issue_archive_sections (organization_id,project_id,work_item_id,kind,subject_id,last_sequence DESC,section_id) WHERE kind<>'';
DROP INDEX CONCURRENTLY IF EXISTS issue_archive_sections_identity;
CREATE INDEX CONCURRENTLY issue_archive_sections_identity ON issue_archive_sections (organization_id,project_id,work_item_id,section_id,archive_revision DESC) WHERE kind<>'';
DROP INDEX CONCURRENTLY IF EXISTS issue_archive_sections_retire;
CREATE INDEX CONCURRENTLY issue_archive_sections_retire ON issue_archive_sections (organization_id,project_id,work_item_id,archive_revision,section_id) WHERE kind<>'' AND NOT retirement_complete;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY issue_archive_sections_retire;
DROP INDEX CONCURRENTLY issue_archive_sections_identity;
DROP INDEX CONCURRENTLY issue_archive_sections_range;
RESET lock_timeout;
