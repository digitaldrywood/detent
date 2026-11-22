-- +goose Up
CREATE TABLE runner_lifecycle (
 organization_id text NOT NULL,
 runner_id text NOT NULL,
 process_id text NOT NULL,
 sequence bigint NOT NULL CHECK(sequence > 0),
 state text NOT NULL CHECK(state IN ('starting','ready','draining','updating','shutting_down','stopped','start_failed')),
 version text NOT NULL,
 build_revision text NOT NULL,
 target_version text NOT NULL DEFAULT '',
 reason text NOT NULL DEFAULT '',
 error_code text NOT NULL DEFAULT '',
 consecutive_failed_starts integer NOT NULL CHECK(consecutive_failed_starts >= 0),
 last_transition_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,runner_id),
 FOREIGN KEY(organization_id,runner_id) REFERENCES runner_identities(organization_id,runner_id) ON DELETE CASCADE
);
CREATE TABLE runner_lifecycle_transitions (
 transition_id bigint GENERATED ALWAYS AS IDENTITY,
 LIKE runner_lifecycle INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
 report_digest text NOT NULL DEFAULT '',
 refusal text NOT NULL DEFAULT '' CHECK(refusal IN ('','invalid_transition','running_build_changed','update_not_offered')),
 PRIMARY KEY(organization_id,runner_id,process_id,sequence,transition_id),
 FOREIGN KEY(organization_id,runner_id) REFERENCES runner_identities(organization_id,runner_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX runner_lifecycle_receipt ON runner_lifecycle_transitions(organization_id,runner_id,process_id,sequence,report_digest) WHERE report_digest <> '';
CREATE INDEX runner_lifecycle_start_deadline ON runner_lifecycle(organization_id,last_transition_at,runner_id) WHERE state IN ('starting','shutting_down');
CREATE INDEX runner_lifecycle_history ON runner_lifecycle_transitions(organization_id,runner_id,last_transition_at DESC,transition_id DESC);
-- +goose StatementBegin
DO $$
DECLARE table_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['runner_lifecycle','runner_lifecycle_transitions'] LOOP
  EXECUTE format('ALTER TABLE %I OWNER TO detent_migration',table_name);
  EXECUTE format('REVOKE ALL ON %I FROM PUBLIC, detent_platform',table_name);
  EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO detent_runtime',table_name);
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',table_name);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',table_name);
  EXECUTE format('CREATE POLICY tenant_isolation ON %I TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting(''detent.organization_id'', true), '''')) WITH CHECK (organization_id = NULLIF(current_setting(''detent.organization_id'', true), ''''))',table_name);
 END LOOP;
END $$;
-- +goose StatementEnd
TRUNCATE TABLE project_onboarding_pages;
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifec_8ea6fd88cf8986bb text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifec_356b72452359a842 bigint[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifecyclereport_state text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifec_b53bfd7e6cf2129b text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifec_31ba2e3e3c1c8032 text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifec_7d88970c1424b483 text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifecyclereport_reason text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_lifec_4018a41db5812948 text[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_last_transition_at timestamptz[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle_value_conse_6206ae7683f5a82b bigint[] NOT NULL DEFAULT '{}';
ALTER TABLE project_onboarding_pages ADD COLUMN page_runners_item_runner_lifecycle boolean[] NOT NULL DEFAULT '{}';
-- +goose Down
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifec_8ea6fd88cf8986bb;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifec_356b72452359a842;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifecyclereport_state;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifec_b53bfd7e6cf2129b;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifec_31ba2e3e3c1c8032;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifec_7d88970c1424b483;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifecyclereport_reason;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_lifec_4018a41db5812948;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_last_transition_at;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle_value_conse_6206ae7683f5a82b;
ALTER TABLE project_onboarding_pages DROP COLUMN page_runners_item_runner_lifecycle;
DROP TABLE runner_lifecycle_transitions;
DROP TABLE runner_lifecycle;
