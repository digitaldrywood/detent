-- +goose Up
ALTER TABLE issues ADD COLUMN last_activity_at TEXT NOT NULL DEFAULT '';

WITH activity(work_item_id, at) AS (
  SELECT native_id, native_updated_at FROM issues
  UNION ALL SELECT native_id, updated_at FROM issues
  UNION ALL SELECT work_item_id, updated_at FROM native_comments
  UNION ALL SELECT work_item_id, recorded_at FROM collaboration_events
  UNION ALL SELECT work_item_id, started_at FROM native_attempts
  UNION ALL SELECT work_item_id, updated_at FROM native_attempts
  UNION ALL SELECT i.native_id, e.recorded_at FROM work_events e JOIN issues i ON i.id = e.issue_id
  UNION ALL
  SELECT l.work_item_id, json_extract(c.record_json, '$.updated_at')
  FROM change_issue_links l JOIN change_requests c ON c.id = l.change_id
  UNION ALL
  SELECT l.work_item_id, json_extract(v.record_json, '$.created_at')
  FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
  UNION ALL
  SELECT l.work_item_id, COALESCE(json_extract(e.record_json, '$.received_at'), json_extract(e.record_json, '$.created_at'))
  FROM change_issue_links l JOIN change_evidence e ON e.change_id = l.change_id
  UNION ALL
  SELECT i.native_id, pr.updated_at FROM issues i JOIN pull_requests pr ON pr.issue_id = i.id
  UNION ALL
  SELECT i.native_id, pr.synchronized_at FROM issues i JOIN pull_requests pr ON pr.issue_id = i.id
  UNION ALL
  SELECT l.work_item_id, pr.updated_at
  FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
  JOIN projects p ON p.organization_id = l.organization_id AND p.id = l.project_id
  JOIN pull_requests pr ON pr.repository_id = p.repository_id AND pr.url = json_extract(v.record_json, '$.external.url')
  UNION ALL
  SELECT l.work_item_id, pr.synchronized_at
  FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
  JOIN projects p ON p.organization_id = l.organization_id AND p.id = l.project_id
  JOIN pull_requests pr ON pr.repository_id = p.repository_id AND pr.url = json_extract(v.record_json, '$.external.url')
)
UPDATE issues SET last_activity_at = (
  SELECT at FROM activity WHERE work_item_id = issues.native_id AND at IS NOT NULL AND at <> ''
  ORDER BY rtrim(at, 'Z') DESC LIMIT 1
);

-- +goose StatementBegin
CREATE TRIGGER issues_activity_insert AFTER INSERT ON issues
BEGIN
  UPDATE issues SET last_activity_at = CASE WHEN rtrim(NEW.native_updated_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.native_updated_at ELSE NEW.updated_at END
  WHERE id = NEW.id AND rtrim(last_activity_at, 'Z') < rtrim(CASE WHEN rtrim(NEW.native_updated_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.native_updated_at ELSE NEW.updated_at END, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER work_events_activity AFTER INSERT ON work_events
BEGIN
  UPDATE issues SET last_activity_at = NEW.recorded_at
  WHERE id = NEW.issue_id AND rtrim(last_activity_at, 'Z') < rtrim(NEW.recorded_at, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER issues_activity_update AFTER UPDATE OF native_updated_at, updated_at ON issues
BEGIN
  UPDATE issues SET last_activity_at = CASE WHEN rtrim(NEW.native_updated_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.native_updated_at ELSE NEW.updated_at END
  WHERE id = NEW.id AND rtrim(last_activity_at, 'Z') < rtrim(CASE WHEN rtrim(NEW.native_updated_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.native_updated_at ELSE NEW.updated_at END, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER collaboration_events_activity AFTER INSERT ON collaboration_events
BEGIN
  UPDATE issues SET last_activity_at = NEW.recorded_at
  WHERE organization_id = NEW.organization_id AND project_id = NEW.project_id AND native_id = NEW.work_item_id AND rtrim(last_activity_at, 'Z') < rtrim(NEW.recorded_at, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_attempts_activity_insert AFTER INSERT ON native_attempts
BEGIN
  UPDATE issues SET last_activity_at = NEW.updated_at
  WHERE organization_id = NEW.organization_id AND project_id = NEW.project_id AND native_id = NEW.work_item_id AND rtrim(last_activity_at, 'Z') < rtrim(NEW.updated_at, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_attempts_activity_update AFTER UPDATE OF updated_at ON native_attempts
BEGIN
  UPDATE issues SET last_activity_at = NEW.updated_at
  WHERE organization_id = NEW.organization_id AND project_id = NEW.project_id AND native_id = NEW.work_item_id AND rtrim(last_activity_at, 'Z') < rtrim(NEW.updated_at, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER change_evidence_activity AFTER INSERT ON change_evidence
BEGIN
  UPDATE issues SET last_activity_at = COALESCE(json_extract(NEW.record_json, '$.received_at'), json_extract(NEW.record_json, '$.created_at'))
  WHERE native_id IN (SELECT work_item_id FROM change_issue_links WHERE change_id = NEW.change_id) AND rtrim(last_activity_at, 'Z') < rtrim(COALESCE(json_extract(NEW.record_json, '$.received_at'), json_extract(NEW.record_json, '$.created_at')), 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER change_requests_activity AFTER UPDATE OF record_json ON change_requests
BEGIN
  UPDATE issues SET last_activity_at = json_extract(NEW.record_json, '$.updated_at')
  WHERE native_id IN (SELECT work_item_id FROM change_issue_links WHERE change_id = NEW.id) AND rtrim(last_activity_at, 'Z') < rtrim(json_extract(NEW.record_json, '$.updated_at'), 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER pull_requests_activity_insert AFTER INSERT ON pull_requests
BEGIN
  UPDATE issues SET last_activity_at = CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END
  WHERE (id = NEW.issue_id OR native_id IN (
    SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
    JOIN projects p ON p.organization_id = l.organization_id AND p.id = l.project_id
    WHERE p.repository_id = NEW.repository_id AND json_extract(v.record_json, '$.external.url') = NEW.url
  )) AND rtrim(last_activity_at, 'Z') < rtrim(CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END, 'Z');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER pull_requests_activity_update AFTER UPDATE OF updated_at ON pull_requests
BEGIN
  UPDATE issues SET last_activity_at = CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END
  WHERE (id = NEW.issue_id OR native_id IN (
    SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
    JOIN projects p ON p.organization_id = l.organization_id AND p.id = l.project_id
    WHERE p.repository_id = NEW.repository_id AND json_extract(v.record_json, '$.external.url') = NEW.url
  )) AND rtrim(last_activity_at, 'Z') < rtrim(CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END, 'Z');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER pull_requests_activity_update;
DROP TRIGGER pull_requests_activity_insert;
DROP TRIGGER change_requests_activity;
DROP TRIGGER change_evidence_activity;
DROP TRIGGER native_attempts_activity_update;
DROP TRIGGER native_attempts_activity_insert;
DROP TRIGGER collaboration_events_activity;
DROP TRIGGER work_events_activity;
DROP TRIGGER issues_activity_update;
DROP TRIGGER issues_activity_insert;
ALTER TABLE issues DROP COLUMN last_activity_at;
