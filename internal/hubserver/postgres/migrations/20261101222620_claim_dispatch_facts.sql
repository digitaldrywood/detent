-- +goose Up
CREATE TABLE issue_claim_facts (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    author_login text NOT NULL,
    repository text NOT NULL,
    assignees text[] NOT NULL,
    dispatch_wait_since timestamptz,
    PRIMARY KEY (organization_id, project_id, work_item_id),
    FOREIGN KEY (organization_id, project_id, work_item_id)
        REFERENCES issues (organization_id, project_id, native_id)
);

CREATE INDEX issue_claim_author_filter_idx ON issue_claim_facts (organization_id, project_id, author_login, work_item_id);
CREATE INDEX issue_claim_repository_filter_idx ON issue_claim_facts (organization_id, project_id, repository, work_item_id);
CREATE INDEX issue_claim_assignees_filter_idx ON issue_claim_facts USING gin (assignees);
CREATE INDEX issue_claim_dispatch_wait_idx ON issue_claim_facts (organization_id, project_id, dispatch_wait_since, work_item_id);

CREATE TABLE queue_entries (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    scope text NOT NULL,
    state text NOT NULL,
    rank text NOT NULL,
    priority_override integer CHECK (priority_override BETWEEN 0 AND 3),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, scope, work_item_id),
    FOREIGN KEY (organization_id, project_id, work_item_id)
        REFERENCES issues (organization_id, project_id, native_id)
);

CREATE INDEX queue_entries_dispatch_idx
    ON queue_entries (organization_id, project_id, scope, priority_override, rank, work_item_id);

ALTER TABLE issue_claim_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_claim_facts FORCE ROW LEVEL SECURITY;
ALTER TABLE queue_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE queue_entries FORCE ROW LEVEL SECURITY;

REVOKE ALL ON issue_claim_facts, queue_entries FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON issue_claim_facts, queue_entries TO detent_runtime, detent_platform;

CREATE POLICY tenant_isolation ON issue_claim_facts TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

CREATE POLICY tenant_isolation ON queue_entries TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

CREATE POLICY platform_access ON queue_entries TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON issue_claim_facts TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE queue_entries;
DROP TABLE issue_claim_facts;
