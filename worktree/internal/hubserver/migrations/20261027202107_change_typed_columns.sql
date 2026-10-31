-- +goose Up
CREATE INDEX issues_change_actor_conversion ON issues(id) WHERE actor_principal_id IS NULL;
ALTER TABLE native_comments ADD COLUMN change_actor_kind TEXT;
ALTER TABLE native_comments ADD COLUMN change_edited_by_kind TEXT;
CREATE INDEX native_comments_change_conversion ON native_comments(id) WHERE change_actor_kind IS NULL;
ALTER TABLE change_requests ADD COLUMN created_at TEXT;
ALTER TABLE change_requests ADD COLUMN updated_at TEXT;
ALTER TABLE change_requests ADD COLUMN landed_head_sha TEXT;
ALTER TABLE change_requests ADD COLUMN landed_merge_sha TEXT;
ALTER TABLE change_requests ADD COLUMN landed_base_ref TEXT;
ALTER TABLE change_requests ADD COLUMN landed_at TEXT;
ALTER TABLE change_versions ADD COLUMN created_at TEXT;
ALTER TABLE change_versions ADD COLUMN attempt_id TEXT;
ALTER TABLE change_versions ADD COLUMN head_sha TEXT;
ALTER TABLE change_versions ADD COLUMN repository TEXT;
ALTER TABLE change_versions ADD COLUMN external_provider TEXT;
ALTER TABLE change_versions ADD COLUMN external_id TEXT;
ALTER TABLE change_evidence ADD COLUMN has_implementation_review INTEGER;
ALTER TABLE change_evidence ADD COLUMN has_validator INTEGER;
ALTER TABLE change_evidence ADD COLUMN has_security_audit INTEGER;
ALTER TABLE change_evidence ADD COLUMN security_audit_landing INTEGER;
ALTER TABLE change_evidence ADD COLUMN check_run_id TEXT;
ALTER TABLE change_evidence ADD COLUMN created_at TEXT;
ALTER TABLE change_evidence ADD COLUMN received_at TEXT;
ALTER TABLE change_evidence ADD COLUMN escape_occurrence_id TEXT;
ALTER TABLE change_evidence ADD COLUMN escape_observed_at TEXT;
ALTER TABLE quality_landings ADD COLUMN landed_at TEXT;
ALTER TABLE change_landing_receipts ADD COLUMN merge_sha TEXT;
ALTER TABLE native_attempts ADD COLUMN has_landing_receipt INTEGER;
CREATE INDEX change_requests_change_conversion ON change_requests(id) WHERE created_at IS NULL;
CREATE INDEX change_versions_change_attempt_unknown ON change_versions(change_id,id) WHERE attempt_id IS NULL;
CREATE INDEX change_versions_change_conversion ON change_versions(id) WHERE head_sha IS NULL;
CREATE INDEX change_evidence_change_unknown ON change_evidence(change_id,version_id,sequence) WHERE has_validator IS NULL;
CREATE INDEX change_evidence_change_conversion ON change_evidence(sequence) WHERE has_validator IS NULL;
CREATE INDEX quality_landings_change_conversion ON quality_landings(version_id) WHERE landed_at IS NULL;
CREATE INDEX change_landing_receipts_change_conversion ON change_landing_receipts(attempt_id) WHERE merge_sha IS NULL;
CREATE INDEX native_attempts_change_conversion ON native_attempts(id) WHERE has_landing_receipt IS NULL;
CREATE INDEX collaboration_events_change_conversion ON collaboration_events(id) WHERE from_state IS NULL;
DROP INDEX IF EXISTS change_requests_activity_latest;
CREATE INDEX change_requests_activity_latest ON change_requests(organization_id,project_id,work_item_id,created_at DESC,id DESC);
DROP INDEX IF EXISTS change_requests_analytics_landed;
CREATE INDEX change_requests_analytics_landed ON change_requests(organization_id,project_id,landed_at,id);
DROP INDEX IF EXISTS change_requests_policy_landing_idx;
CREATE INDEX change_requests_policy_landing_idx ON change_requests(organization_id,project_id,landed_merge_sha,landed_base_ref);
DROP INDEX IF EXISTS change_versions_attempt_idx;
CREATE INDEX change_versions_attempt_idx ON change_versions(attempt_id);
DROP INDEX IF EXISTS change_evidence_approval_idx;
CREATE INDEX change_evidence_approval_idx ON change_evidence(change_id,kind,claim_review_decision,version_id);
DROP INDEX IF EXISTS change_evidence_analytics_review;
CREATE INDEX change_evidence_analytics_review ON change_evidence(created_at,sequence,change_id,version_id) WHERE kind='review' AND claim_review_decision='changes_requested';
DROP INDEX IF EXISTS change_evidence_analytics_escape;
CREATE INDEX change_evidence_analytics_escape ON change_evidence(escape_observed_at,sequence,change_id,version_id) WHERE kind='discussion' AND escape_occurrence_id<>'';
DROP INDEX IF EXISTS quality_landings_analytics_landed;
CREATE INDEX quality_landings_analytics_landed ON quality_landings(landed_at,version_id);
DROP TRIGGER change_versions_immutable;
DROP TRIGGER change_evidence_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OF id,change_id,number,record_json ON change_versions
BEGIN SELECT RAISE(ABORT, 'change versions are immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE OF sequence,change_id,version_id,kind,source_key,record_json ON change_evidence
BEGIN SELECT RAISE(ABORT, 'change evidence is immutable'); END;
-- +goose StatementEnd
DROP TRIGGER change_evidence_activity;
DROP TRIGGER change_requests_activity;
DROP TRIGGER pull_requests_activity_insert;
DROP TRIGGER pull_requests_activity_update;
-- +goose StatementBegin
CREATE TRIGGER change_evidence_activity AFTER UPDATE OF created_at,received_at ON change_evidence BEGIN
 UPDATE issues SET last_activity_at=COALESCE(NEW.received_at,NEW.created_at)
 WHERE (organization_id,project_id,native_id) IN (SELECT organization_id,project_id,work_item_id FROM change_issue_links WHERE change_id=NEW.change_id)
 AND rtrim(last_activity_at,'Z')<rtrim(COALESCE(NEW.received_at,NEW.created_at),'Z');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_requests_activity AFTER UPDATE OF updated_at ON change_requests BEGIN
 UPDATE issues SET last_activity_at=NEW.updated_at
 WHERE (organization_id,project_id,native_id) IN (SELECT organization_id,project_id,work_item_id FROM change_issue_links WHERE change_id=NEW.id)
 AND rtrim(last_activity_at,'Z')<rtrim(NEW.updated_at,'Z');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER pull_requests_activity_insert AFTER INSERT ON pull_requests BEGIN
 UPDATE issues SET last_activity_at=CASE WHEN rtrim(NEW.synchronized_at,'Z')>rtrim(NEW.updated_at,'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END
 WHERE (id=NEW.issue_id OR (organization_id,project_id,native_id) IN (
 SELECT l.organization_id,l.project_id,l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.organization_id=l.organization_id AND p.id=l.project_id
 WHERE p.repository_id=NEW.repository_id AND v.external_url=NEW.url))
 AND rtrim(last_activity_at,'Z')<rtrim(CASE WHEN rtrim(NEW.synchronized_at,'Z')>rtrim(NEW.updated_at,'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END,'Z');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER pull_requests_activity_update AFTER UPDATE ON pull_requests BEGIN
 UPDATE issues SET last_activity_at=CASE WHEN rtrim(NEW.synchronized_at,'Z')>rtrim(NEW.updated_at,'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END
 WHERE (id=NEW.issue_id OR (organization_id,project_id,native_id) IN (
 SELECT l.organization_id,l.project_id,l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.organization_id=l.organization_id AND p.id=l.project_id
 WHERE p.repository_id=NEW.repository_id AND v.external_url=NEW.url))
 AND rtrim(last_activity_at,'Z')<rtrim(CASE WHEN rtrim(NEW.synchronized_at,'Z')>rtrim(NEW.updated_at,'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END,'Z');
END;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
