-- +goose Up
DROP TRIGGER native_board_project_policies_update;
DROP TRIGGER native_board_change_review_policies_update;

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_update AFTER UPDATE ON project_policies
WHEN NEW.policy_id IS NOT OLD.policy_id
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id||'/'||l.project_id=NEW.scope AND (
  (SELECT json_extract(metadata_json,'$.gates') FROM policy_revisions WHERE scope=NEW.scope AND policy_id=NEW.policy_id)
  IS NOT (SELECT json_extract(metadata_json,'$.gates') FROM policy_revisions WHERE scope=OLD.scope AND policy_id=OLD.policy_id)
  OR NOT EXISTS (
   SELECT 1 FROM change_requests c JOIN change_versions v ON v.id=json_extract(c.record_json,'$.current_version_id') AND v.change_id=c.id
   JOIN change_review_policies r ON r.organization_id=l.organization_id AND r.project_id=l.project_id
   JOIN policy_revisions p ON p.scope=NEW.scope AND p.policy_id=NEW.policy_id
   WHERE c.id=l.change_id
   AND json_extract(v.record_json,'$.policy_id')=json_extract(v.record_json,'$.policy.policy_id')
   AND COALESCE(json_extract(v.record_json,'$.policy_id'),'')<>''
   AND COALESCE(json_extract(v.record_json,'$.review_policy.review_policy_id'),'')<>''
   AND COALESCE(json_extract(r.policy_json,'$.review_policy_id'),'')<>''
   AND json_extract(v.record_json,'$.policy.gates')=json_extract(p.metadata_json,'$.gates')
   AND json_remove(json_extract(v.record_json,'$.review_policy'),'$.review_policy_id','$.policy_id')
       =json_remove(r.policy_json,'$.review_policy_id','$.policy_id')
   AND json_type(r.policy_json,'$.required_checks')='array'
   AND json_array_length(r.policy_json,'$.required_checks')=0
   AND json_extract(p.metadata_json,'$.gates.required_checks')=0
   AND (json_extract(p.metadata_json,'$.gates.kind')<>'human_review' OR json_extract(r.policy_json,'$.require_review')=1)
  )
 ) ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_update AFTER UPDATE ON change_review_policies
WHEN NEW.policy_json IS NOT OLD.policy_json
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id=NEW.organization_id AND l.project_id=NEW.project_id AND (
  json_remove(NEW.policy_json,'$.review_policy_id','$.policy_id') IS NOT json_remove(OLD.policy_json,'$.review_policy_id','$.policy_id')
  OR NOT EXISTS (
   SELECT 1 FROM change_requests c JOIN change_versions v ON v.id=json_extract(c.record_json,'$.current_version_id') AND v.change_id=c.id
   JOIN change_review_policies r ON r.organization_id=l.organization_id AND r.project_id=l.project_id
   JOIN project_policies a ON a.scope=l.organization_id||'/'||l.project_id
   JOIN policy_revisions p ON p.scope=a.scope AND p.policy_id=a.policy_id
   WHERE c.id=l.change_id
   AND json_extract(v.record_json,'$.policy_id')=json_extract(v.record_json,'$.policy.policy_id')
   AND COALESCE(json_extract(v.record_json,'$.policy_id'),'')<>''
   AND COALESCE(json_extract(v.record_json,'$.review_policy.review_policy_id'),'')<>''
   AND COALESCE(json_extract(r.policy_json,'$.review_policy_id'),'')<>''
   AND json_extract(v.record_json,'$.policy.gates')=json_extract(p.metadata_json,'$.gates')
   AND json_remove(json_extract(v.record_json,'$.review_policy'),'$.review_policy_id','$.policy_id')
       =json_remove(r.policy_json,'$.review_policy_id','$.policy_id')
   AND json_type(r.policy_json,'$.required_checks')='array'
   AND json_array_length(r.policy_json,'$.required_checks')=0
   AND json_extract(p.metadata_json,'$.gates.required_checks')=0
   AND (json_extract(p.metadata_json,'$.gates.kind')<>'human_review' OR json_extract(r.policy_json,'$.require_review')=1)
  )
 ) ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER native_board_project_policies_update;
DROP TRIGGER native_board_change_review_policies_update;

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_update AFTER UPDATE ON project_policies
WHEN NEW.policy_id IS NOT OLD.policy_id
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id||'/'||l.project_id=NEW.scope
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_update AFTER UPDATE ON change_review_policies
WHEN NEW.policy_json IS NOT OLD.policy_json
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id=NEW.organization_id AND l.project_id=NEW.project_id
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

