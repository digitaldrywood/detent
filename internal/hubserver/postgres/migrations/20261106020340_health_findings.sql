-- +goose Up
CREATE TABLE health_findings (
 organization_id text NOT NULL REFERENCES organizations(organization_id) ON DELETE CASCADE,
 id text NOT NULL,
 sequence bigint GENERATED ALWAYS AS IDENTITY,
 fingerprint text NOT NULL,
 signal text NOT NULL,
 class text NOT NULL,
 subject_kind text NOT NULL,
 subject_id text NOT NULL,
 opened_at timestamptz NOT NULL,
 last_seen_at timestamptz NOT NULL,
 resolved_at timestamptz,
 severity text NOT NULL,
 summary text NOT NULL,
 next_action text NOT NULL,
 evidence_object_key text NOT NULL,
 evidence_sha256 text NOT NULL,
 evidence_bytes bigint NOT NULL,
 evidence_content_bytes bigint NOT NULL,
 email_unavailable boolean NOT NULL DEFAULT false,
 slack_unavailable_at timestamptz,
 slack_status_code integer NOT NULL DEFAULT 0,
 PRIMARY KEY (organization_id,id),
 UNIQUE (organization_id,sequence)
);
CREATE INDEX health_findings_summary ON health_findings(organization_id,opened_at,id) WHERE resolved_at IS NULL AND severity='attention';
CREATE TABLE health_finding_projects (
 organization_id text NOT NULL,
 finding_id text NOT NULL,
 project_id text NOT NULL,
 PRIMARY KEY (organization_id,finding_id,project_id),
 FOREIGN KEY (organization_id,finding_id) REFERENCES health_findings(organization_id,id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
CREATE INDEX health_finding_projects_project ON health_finding_projects(organization_id,project_id,finding_id);
CREATE TABLE health_finding_issues (
 organization_id text NOT NULL,
 finding_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 PRIMARY KEY (organization_id,finding_id,project_id,work_item_id),
 FOREIGN KEY (organization_id,finding_id,project_id) REFERENCES health_finding_projects(organization_id,finding_id,project_id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id,project_id,work_item_id) REFERENCES issues(organization_id,project_id,native_id) ON DELETE CASCADE
);
CREATE TABLE health_detector_ticks (
 organization_id text PRIMARY KEY REFERENCES organizations(organization_id) ON DELETE CASCADE,
 last_tick_at timestamptz NOT NULL
);
CREATE TABLE health_baseline_unavailable (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 signal text NOT NULL,
 lane text NOT NULL,
 PRIMARY KEY (organization_id,project_id,signal,lane),
 FOREIGN KEY (organization_id,project_id) REFERENCES projects(organization_id,id) ON DELETE CASCADE
);
-- +goose StatementBegin
DO $$ DECLARE relation text; BEGIN
 FOREACH relation IN ARRAY ARRAY['health_findings','health_finding_projects','health_finding_issues','health_detector_ticks','health_baseline_unavailable'] LOOP
  EXECUTE format('ALTER TABLE %I OWNER TO detent_migration',relation);
  EXECUTE format('REVOKE ALL ON %I FROM PUBLIC,detent_platform',relation);
  EXECUTE format('GRANT SELECT ON %I TO detent_runtime',relation);
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',relation);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',relation);
  EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting(''detent.organization_id'',true),'''')) WITH CHECK (organization_id=NULLIF(current_setting(''detent.organization_id'',true),''''))',relation);
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 RAISE EXCEPTION 'health findings require an explicit retained-data migration';
END $$;
-- +goose StatementEnd
