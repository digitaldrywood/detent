-- +goose Up
CREATE INDEX health_issue_work_item ON health_finding_issues(organization_id,project_id,work_item_id,finding_id);
CREATE INDEX health_issue_human_handoff ON collaboration_events(organization_id,project_id,work_item_id) WHERE type='workflow.transitioned' AND actor_kind='human' AND from_state='Backlog' AND to_state<>'Backlog';

CREATE TABLE health_issue_reports (
 organization_id text NOT NULL,
 finding_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 publication_sequence bigint GENERATED ALWAYS AS IDENTITY,
 intake_fingerprint text NOT NULL CHECK(intake_fingerprint<>''),
 reported_evidence_digest text NOT NULL CHECK(reported_evidence_digest ~ '^[0-9a-f]{64}$'),
 reported_resolved_at timestamptz,
 PRIMARY KEY(organization_id,finding_id,project_id,intake_fingerprint),
 FOREIGN KEY(organization_id,finding_id,project_id,work_item_id) REFERENCES health_finding_issues(organization_id,finding_id,project_id,work_item_id) ON DELETE CASCADE
);
CREATE INDEX health_issue_latest_intake ON health_issue_reports(organization_id,project_id,intake_fingerprint,publication_sequence DESC);
ALTER TABLE health_issue_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE health_issue_reports FORCE ROW LEVEL SECURITY;
REVOKE ALL ON health_issue_reports FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON health_issue_reports TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON health_issue_reports TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON health_issue_reports TO detent_platform USING(true) WITH CHECK(true);

CREATE TABLE health_issue_queue (
 organization_id text NOT NULL,
 finding_id text NOT NULL,
 project_id text NOT NULL,
 source_revision bigint NOT NULL CHECK(source_revision>0),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 source_digest text NOT NULL CHECK(source_digest ~ '^[0-9a-f]{64}$'),
 after_fingerprint text NOT NULL DEFAULT '',
 pending boolean NOT NULL DEFAULT true,
 next_check_at timestamptz NOT NULL,
 checked_at timestamptz,
 PRIMARY KEY(organization_id,finding_id,project_id),
 FOREIGN KEY(organization_id,finding_id,project_id) REFERENCES health_finding_projects(organization_id,finding_id,project_id) ON DELETE CASCADE
);
CREATE INDEX health_issue_queue_pending ON health_issue_queue(organization_id,next_check_at,checked_at NULLS FIRST,finding_id,project_id) WHERE pending;
ALTER TABLE health_issue_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE health_issue_queue FORCE ROW LEVEL SECURITY;
REVOKE ALL ON health_issue_queue FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON health_issue_queue TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON health_issue_queue TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON health_issue_queue TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
DROP TABLE health_issue_queue;
DROP TABLE health_issue_reports;
DROP INDEX health_issue_work_item;
DROP INDEX health_issue_human_handoff;
