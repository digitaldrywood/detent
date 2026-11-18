-- +goose Up
CREATE TABLE issue_dependency_reasons (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    work_item_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('blocked','unblocks')),
    reason text NOT NULL CHECK (char_length(reason)<=140),
    basis text NOT NULL CHECK (basis ~ '^[a-f0-9]{64}$'),
    revision bigint NOT NULL CHECK (revision>0),
    PRIMARY KEY (organization_id,project_id,work_item_id,kind),
    FOREIGN KEY (organization_id,project_id,work_item_id)
        REFERENCES issues (organization_id,project_id,native_id) ON DELETE CASCADE
);
ALTER TABLE issue_dependency_reasons OWNER TO detent_migration;
REVOKE ALL ON issue_dependency_reasons FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_dependency_reasons TO detent_runtime;
ALTER TABLE issue_dependency_reasons ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_dependency_reasons FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON issue_dependency_reasons TO detent_runtime,detent_migration
    USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
    WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'dependency reason records require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
