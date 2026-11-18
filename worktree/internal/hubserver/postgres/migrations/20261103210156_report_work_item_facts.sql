-- +goose Up
ALTER TABLE issues
    ADD COLUMN issue_type text NOT NULL DEFAULT '' CHECK (issue_type IN ('', 'bug', 'change_request')),
    ADD COLUMN priority smallint CHECK (priority BETWEEN 0 AND 3);

ALTER TABLE change_requests
    ADD COLUMN landed_version_id text,
    ADD COLUMN landed_at timestamptz,
    ADD COLUMN landed_head_sha text,
    ADD COLUMN landed_merge_sha text,
    ADD COLUMN landed_base_ref text;

CREATE INDEX issues_reports_created_idx
    ON issues (organization_id, project_id, created_at, native_id);
CREATE INDEX collaboration_events_reports_window_idx
    ON collaboration_events (organization_id, project_id, recorded_at, work_item_id)
    WHERE type = 'workflow.transitioned';
CREATE INDEX change_requests_reports_landed_idx
    ON change_requests (organization_id, project_id, landed_at, work_item_id)
    WHERE landed_at IS NOT NULL;
CREATE INDEX native_attempts_reports_window_idx
    ON native_attempts (organization_id, updated_at, issue_id)
    INCLUDE (status, started_at, lease_id);

CREATE INDEX native_attempts_reports_started_idx ON native_attempts(organization_id,issue_id,started_at,id);

CREATE TABLE report_event_facts (
    organization_id text NOT NULL,
    event_id text NOT NULL,
    duration_seconds double precision NOT NULL DEFAULT 0 CHECK (duration_seconds>=0),
    run_sequence bigint NOT NULL DEFAULT 0,
    run_status text NOT NULL DEFAULT '',
    role text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    effort text NOT NULL DEFAULT '',
    failed boolean NOT NULL DEFAULT false,
    failure_class text NOT NULL DEFAULT '',
    failure_kind text NOT NULL DEFAULT '',
    landing_receipt boolean NOT NULL DEFAULT false,
    landed boolean NOT NULL DEFAULT false,
    landing_change_id text NOT NULL DEFAULT '',
    rework_class text NOT NULL DEFAULT '',
    rework_kind text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id,event_id),
    FOREIGN KEY (organization_id,event_id) REFERENCES collaboration_events(organization_id,id) ON DELETE CASCADE
);
ALTER TABLE report_event_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_event_facts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON report_event_facts FROM PUBLIC;
GRANT SELECT,INSERT ON report_event_facts TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON report_event_facts TO detent_runtime,detent_migration
    USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON report_event_facts TO detent_platform USING (true) WITH CHECK (true);
CREATE INDEX collaboration_events_report_attempt_idx
    ON collaboration_events(organization_id,attempt_id,sequence DESC) WHERE attempt_id<>'';

CREATE TABLE report_escape_facts (
 organization_id text NOT NULL,
 change_id text NOT NULL,
 evidence_sequence bigint NOT NULL,
 occurrence_id text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('revert','scheduled_failure','reopened')),
 cause text NOT NULL CHECK(cause IN ('underspecified_issue','missing_criterion','validator_miss','infrastructure')),
 observed_at timestamptz NOT NULL,
 instance_id text NOT NULL DEFAULT '',
 human_override boolean NOT NULL DEFAULT false,
 PRIMARY KEY(organization_id,change_id,evidence_sequence),
 FOREIGN KEY(organization_id,change_id,evidence_sequence) REFERENCES change_evidence(organization_id,change_id,sequence) ON DELETE CASCADE
);
ALTER TABLE report_escape_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_escape_facts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON report_escape_facts FROM PUBLIC;
GRANT SELECT,INSERT ON report_escape_facts TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON report_escape_facts TO detent_runtime,detent_migration USING(organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON report_escape_facts TO detent_platform USING(true) WITH CHECK(true);
CREATE INDEX report_escape_facts_window_idx ON report_escape_facts(organization_id,observed_at,change_id);

-- +goose Down
DROP INDEX native_attempts_reports_started_idx;
DROP TABLE report_escape_facts;
DROP INDEX collaboration_events_report_attempt_idx;
DROP TABLE report_event_facts;
DROP INDEX change_requests_reports_landed_idx;
DROP INDEX native_attempts_reports_window_idx;
DROP INDEX collaboration_events_reports_window_idx;
DROP INDEX issues_reports_created_idx;
ALTER TABLE change_requests DROP COLUMN landed_base_ref, DROP COLUMN landed_merge_sha, DROP COLUMN landed_head_sha, DROP COLUMN landed_at, DROP COLUMN landed_version_id;
ALTER TABLE issues DROP COLUMN priority, DROP COLUMN issue_type;
