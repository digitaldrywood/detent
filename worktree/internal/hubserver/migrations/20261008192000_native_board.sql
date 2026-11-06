-- +goose Up
CREATE TABLE native_board_dirty (work_item_id TEXT PRIMARY KEY);
CREATE TABLE native_board_items (
 work_item_id TEXT PRIMARY KEY, organization_id TEXT NOT NULL, project_id TEXT NOT NULL,
 state TEXT NOT NULL, archived INTEGER NOT NULL, terminal INTEGER NOT NULL, running INTEGER NOT NULL,
 lane_rank INTEGER NOT NULL, priority_rank INTEGER NOT NULL, activity_missing INTEGER NOT NULL,
 activity_at TEXT NOT NULL, identifier TEXT NOT NULL, closed_at TEXT NOT NULL,
 expires_at TEXT NOT NULL, record_json TEXT NOT NULL CHECK(json_valid(record_json))
);
CREATE INDEX native_board_lane_idx ON native_board_items(organization_id, project_id, archived, state, priority_rank, activity_missing, activity_at DESC, identifier);
CREATE INDEX native_board_order_idx ON native_board_items(organization_id, project_id, archived, lane_rank, state, priority_rank, activity_missing, activity_at DESC, identifier);
CREATE INDEX native_board_expiry_idx ON native_board_items(expires_at) WHERE running=1;
CREATE INDEX native_board_completed_idx ON native_board_items(organization_id, project_id, archived, terminal, closed_at);
CREATE TABLE native_board_counts (
 organization_id TEXT NOT NULL, project_id TEXT NOT NULL, archived INTEGER NOT NULL, state TEXT NOT NULL,
 total INTEGER NOT NULL, running INTEGER NOT NULL,
 PRIMARY KEY(organization_id, project_id, archived, state)
);
CREATE TABLE native_board_deltas (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, organization_id TEXT NOT NULL, project_id TEXT NOT NULL,
 previous_json TEXT, current_json TEXT
);
CREATE INDEX native_board_replay_idx ON native_board_deltas(organization_id, project_id, sequence);
CREATE TABLE native_board_sequences (
 organization_id TEXT NOT NULL, project_id TEXT NOT NULL, sequence INTEGER NOT NULL DEFAULT 0, replay_floor INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(organization_id, project_id)
);
INSERT INTO native_board_dirty SELECT native_id FROM issues WHERE native_id IS NOT NULL;

-- +goose StatementBegin
CREATE TRIGGER native_board_issues_insert AFTER INSERT ON issues
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.native_id WHERE NEW.native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.dependent_issue_id=i.id WHERE d.blocker_issue_id=NEW.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_issues_update AFTER UPDATE ON issues
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.native_id WHERE NEW.native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.dependent_issue_id=i.id WHERE d.blocker_issue_id=NEW.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_issues_delete AFTER DELETE ON issues
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT OLD.native_id WHERE OLD.native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.dependent_issue_id=i.id WHERE d.blocker_issue_id=OLD.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_native_attempts_insert AFTER INSERT ON native_attempts
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_native_attempts_update AFTER UPDATE ON native_attempts
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_native_attempts_delete AFTER DELETE ON native_attempts
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT OLD.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_native_comments_insert AFTER INSERT ON native_comments
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_native_comments_update AFTER UPDATE ON native_comments
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_native_comments_delete AFTER DELETE ON native_comments
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT OLD.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_collaboration_events_insert AFTER INSERT ON collaboration_events
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_collaboration_events_update AFTER UPDATE ON collaboration_events
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_collaboration_events_delete AFTER DELETE ON collaboration_events
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT OLD.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_queue_entries_insert AFTER INSERT ON queue_entries
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_queue_entries_update AFTER UPDATE ON queue_entries
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_queue_entries_delete AFTER DELETE ON queue_entries
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_leases_insert AFTER INSERT ON leases
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_leases_update AFTER UPDATE ON leases
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_leases_delete AFTER DELETE ON leases
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_work_events_insert AFTER INSERT ON work_events
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_work_events_update AFTER UPDATE ON work_events
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_work_events_delete AFTER DELETE ON work_events
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_dependency_insert AFTER INSERT ON issue_dependencies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.dependent_issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_dependency_delete AFTER DELETE ON issue_dependencies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.dependent_issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_requests_insert AFTER INSERT ON change_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_requests_update AFTER UPDATE ON change_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_requests_delete AFTER DELETE ON change_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=OLD.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_versions_insert AFTER INSERT ON change_versions
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_versions_update AFTER UPDATE ON change_versions
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_versions_delete AFTER DELETE ON change_versions
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=OLD.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_evidence_insert AFTER INSERT ON change_evidence
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_evidence_update AFTER UPDATE ON change_evidence
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_evidence_delete AFTER DELETE ON change_evidence
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=OLD.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_issue_links_insert AFTER INSERT ON change_issue_links
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_issue_links_update AFTER UPDATE ON change_issue_links
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=NEW.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_issue_links_delete AFTER DELETE ON change_issue_links
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT work_item_id FROM change_issue_links WHERE change_id=OLD.change_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT OLD.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_pull_requests_insert AFTER INSERT ON pull_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL
 UNION SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.id=l.project_id WHERE p.repository_id=NEW.repository_id AND json_extract(v.record_json, '$.external.url')=NEW.url) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_pull_requests_update AFTER UPDATE ON pull_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL
 UNION SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.id=l.project_id WHERE p.repository_id=NEW.repository_id AND json_extract(v.record_json, '$.external.url')=NEW.url) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_pull_requests_delete AFTER DELETE ON pull_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.issue_id AND native_id IS NOT NULL
 UNION SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.id=l.project_id WHERE p.repository_id=OLD.repository_id AND json_extract(v.record_json, '$.external.url')=OLD.url) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_projects_update AFTER UPDATE ON projects
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_workflow_states_update AFTER UPDATE ON workflow_states
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_counts_insert AFTER INSERT ON native_board_items
BEGIN
 INSERT INTO native_board_counts VALUES(NEW.organization_id,NEW.project_id,NEW.archived,NEW.state,1,NEW.running)
 ON CONFLICT(organization_id,project_id,archived,state) DO UPDATE SET total=total+excluded.total, running=running+excluded.running;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_counts_delete AFTER DELETE ON native_board_items
BEGIN
 INSERT INTO native_board_counts VALUES(OLD.organization_id,OLD.project_id,OLD.archived,OLD.state,-1,-OLD.running)
 ON CONFLICT(organization_id,project_id,archived,state) DO UPDATE SET total=total+excluded.total, running=running+excluded.running;
END;
-- +goose StatementEnd


-- +goose StatementBegin
CREATE TRIGGER native_board_linked_issue_sources_insert AFTER INSERT ON linked_issue_sources
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_linked_issue_sources_update AFTER UPDATE ON linked_issue_sources
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_linked_issue_sources_delete AFTER DELETE ON linked_issue_sources
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT OLD.work_item_id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_insert AFTER INSERT ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_update AFTER UPDATE ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_delete AFTER DELETE ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=OLD.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_insert AFTER INSERT ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE organization_id||'/'||project_id=NEW.scope AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_update AFTER UPDATE ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE organization_id||'/'||project_id=NEW.scope AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_delete AFTER DELETE ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE organization_id||'/'||project_id=OLD.scope AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER native_board_linked_issue_sources_insert;
DROP TRIGGER native_board_linked_issue_sources_update;
DROP TRIGGER native_board_linked_issue_sources_delete;
DROP TRIGGER native_board_change_review_policies_insert;
DROP TRIGGER native_board_change_review_policies_update;
DROP TRIGGER native_board_change_review_policies_delete;
DROP TRIGGER native_board_project_policies_insert;
DROP TRIGGER native_board_project_policies_update;
DROP TRIGGER native_board_project_policies_delete;

DROP TRIGGER native_board_counts_delete;
DROP TRIGGER native_board_counts_insert;
DROP TRIGGER native_board_workflow_states_update;
DROP TRIGGER native_board_projects_update;
DROP TRIGGER native_board_pull_requests_delete;
DROP TRIGGER native_board_pull_requests_update;
DROP TRIGGER native_board_pull_requests_insert;
DROP TRIGGER native_board_change_issue_links_delete;
DROP TRIGGER native_board_change_issue_links_update;
DROP TRIGGER native_board_change_issue_links_insert;
DROP TRIGGER native_board_change_evidence_delete;
DROP TRIGGER native_board_change_evidence_update;
DROP TRIGGER native_board_change_evidence_insert;
DROP TRIGGER native_board_change_versions_delete;
DROP TRIGGER native_board_change_versions_update;
DROP TRIGGER native_board_change_versions_insert;
DROP TRIGGER native_board_change_requests_delete;
DROP TRIGGER native_board_change_requests_update;
DROP TRIGGER native_board_change_requests_insert;
DROP TRIGGER native_board_dependency_delete;
DROP TRIGGER native_board_dependency_insert;
DROP TRIGGER native_board_work_events_delete;
DROP TRIGGER native_board_work_events_update;
DROP TRIGGER native_board_work_events_insert;
DROP TRIGGER native_board_leases_delete;
DROP TRIGGER native_board_leases_update;
DROP TRIGGER native_board_leases_insert;
DROP TRIGGER native_board_queue_entries_delete;
DROP TRIGGER native_board_queue_entries_update;
DROP TRIGGER native_board_queue_entries_insert;
DROP TRIGGER native_board_collaboration_events_delete;
DROP TRIGGER native_board_collaboration_events_update;
DROP TRIGGER native_board_collaboration_events_insert;
DROP TRIGGER native_board_native_comments_delete;
DROP TRIGGER native_board_native_comments_update;
DROP TRIGGER native_board_native_comments_insert;
DROP TRIGGER native_board_native_attempts_delete;
DROP TRIGGER native_board_native_attempts_update;
DROP TRIGGER native_board_native_attempts_insert;
DROP TRIGGER native_board_issues_delete;
DROP TRIGGER native_board_issues_update;
DROP TRIGGER native_board_issues_insert;
DROP TABLE native_board_sequences;
DROP TABLE native_board_deltas;
DROP TABLE native_board_counts;
DROP TABLE native_board_items;
DROP TABLE native_board_dirty;
