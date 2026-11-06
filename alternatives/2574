-- +goose Up
ALTER TABLE issues ADD CONSTRAINT issues_project_id_id_key UNIQUE (organization_id, project_id, id);

CREATE TABLE issue_dependencies (
    organization_id text NOT NULL,
    dependent_project_id text NOT NULL,
    dependent_issue_id bigint NOT NULL,
    blocker_project_id text NOT NULL,
    blocker_issue_id bigint NOT NULL,
    provenance text NOT NULL DEFAULT 'native',
    source_version text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, dependent_project_id, dependent_issue_id, blocker_project_id, blocker_issue_id),
    CHECK (dependent_issue_id <> blocker_issue_id),
    FOREIGN KEY (organization_id, dependent_project_id, dependent_issue_id)
        REFERENCES issues (organization_id, project_id, id),
    FOREIGN KEY (organization_id, blocker_project_id, blocker_issue_id)
        REFERENCES issues (organization_id, project_id, id)
);

CREATE INDEX issue_dependencies_blocker_idx
    ON issue_dependencies (organization_id, blocker_project_id, blocker_issue_id, dependent_project_id, dependent_issue_id);

CREATE TABLE issue_relationships (
    organization_id text NOT NULL,
    first_project_id text NOT NULL,
    first_issue_id bigint NOT NULL,
    second_project_id text NOT NULL,
    second_issue_id bigint NOT NULL,
    relationship_type text NOT NULL CHECK (relationship_type = 'related'),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization_id, first_project_id, first_issue_id, second_project_id, second_issue_id, relationship_type),
    CHECK (first_issue_id < second_issue_id),
    FOREIGN KEY (organization_id, first_project_id, first_issue_id)
        REFERENCES issues (organization_id, project_id, id),
    FOREIGN KEY (organization_id, second_project_id, second_issue_id)
        REFERENCES issues (organization_id, project_id, id)
);

CREATE INDEX issue_relationships_second_idx
    ON issue_relationships (organization_id, second_project_id, second_issue_id, first_project_id, first_issue_id);

CREATE TABLE issue_labels (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    label text NOT NULL CHECK (label <> ''),
    PRIMARY KEY (organization_id, project_id, work_item_id, label),
    FOREIGN KEY (organization_id, project_id, work_item_id)
        REFERENCES issues (organization_id, project_id, native_id)
);

CREATE INDEX issue_labels_catalog_idx
    ON issue_labels (organization_id, project_id, label, work_item_id);

ALTER TABLE issue_dependencies ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_dependencies FORCE ROW LEVEL SECURITY;
ALTER TABLE issue_relationships ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_relationships FORCE ROW LEVEL SECURITY;
ALTER TABLE issue_labels ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_labels FORCE ROW LEVEL SECURITY;

REVOKE ALL ON issue_dependencies, issue_relationships, issue_labels FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON issue_dependencies, issue_relationships, issue_labels TO detent_runtime, detent_platform;

CREATE POLICY tenant_isolation ON issue_dependencies TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON issue_relationships TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON issue_labels TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON issue_dependencies TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON issue_relationships TO detent_platform USING (true) WITH CHECK (true);
CREATE POLICY platform_access ON issue_labels TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE issue_labels;
DROP TABLE issue_relationships;
DROP TABLE issue_dependencies;
ALTER TABLE issues DROP CONSTRAINT issues_project_id_id_key;
