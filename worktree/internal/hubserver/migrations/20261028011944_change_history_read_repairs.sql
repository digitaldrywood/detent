-- +goose Up
ALTER TABLE native_attempts ADD COLUMN activity_identity_known INTEGER;
ALTER TABLE native_attempts ADD COLUMN finalization_external_url TEXT;
ALTER TABLE native_attempts ADD COLUMN checkpoint_change_id TEXT;
CREATE INDEX native_attempts_activity_identity_conversion ON native_attempts(id) WHERE activity_identity_known IS NULL;
CREATE INDEX change_requests_policy_unknown ON change_requests(organization_id,project_id,id) WHERE created_at IS NULL;
DROP INDEX change_requests_policy_landing_idx;
CREATE INDEX change_requests_policy_landing_idx ON change_requests(organization_id,project_id,landed_merge_sha,landed_base_ref,id);
CREATE INDEX change_versions_change_unknown ON change_versions(change_id) WHERE head_sha IS NULL;
CREATE INDEX collaboration_events_change_unknown ON collaboration_events(organization_id,project_id,work_item_id) WHERE type='workflow.transitioned' AND (from_state IS NULL OR to_state IS NULL);
CREATE INDEX change_landing_receipts_merge ON change_landing_receipts(merge_sha,version_id);
-- +goose StatementBegin
CREATE TRIGGER change_versions_linked_activity AFTER UPDATE OF external_url ON change_versions
WHEN NEW.external_url IS NOT NULL AND NEW.external_url<>''
BEGIN
 UPDATE issues SET last_activity_at=(
 SELECT CASE WHEN julianday(pr.synchronized_at)>julianday(pr.updated_at) THEN pr.synchronized_at ELSE pr.updated_at END
 FROM projects p JOIN pull_requests pr ON pr.repository_id=p.repository_id AND pr.url=NEW.external_url
 WHERE p.organization_id=issues.organization_id AND p.id=issues.project_id
 ORDER BY max(julianday(pr.synchronized_at),julianday(pr.updated_at)) DESC LIMIT 1)
 WHERE (organization_id,project_id,native_id) IN (SELECT organization_id,project_id,work_item_id FROM change_issue_links WHERE change_id=NEW.change_id)
 AND EXISTS(SELECT 1 FROM projects p JOIN pull_requests pr ON pr.repository_id=p.repository_id AND pr.url=NEW.external_url
 WHERE p.organization_id=issues.organization_id AND p.id=issues.project_id AND max(julianday(pr.synchronized_at),julianday(pr.updated_at))>julianday(issues.last_activity_at));
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER change_versions_linked_activity;
DROP INDEX change_landing_receipts_merge;
DROP INDEX collaboration_events_change_unknown;
DROP INDEX change_versions_change_unknown;
DROP INDEX change_requests_policy_landing_idx;
CREATE INDEX change_requests_policy_landing_idx ON change_requests(organization_id,project_id,landed_merge_sha,landed_base_ref);
DROP INDEX change_requests_policy_unknown;
DROP INDEX native_attempts_activity_identity_conversion;
ALTER TABLE native_attempts DROP COLUMN checkpoint_change_id;
ALTER TABLE native_attempts DROP COLUMN finalization_external_url;
ALTER TABLE native_attempts DROP COLUMN activity_identity_known;
