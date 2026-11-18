-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE TABLE github_pull_request_refresh (
 organization_id text NOT NULL REFERENCES organizations(organization_id) ON DELETE CASCADE,
 project_id text NOT NULL,
 change_id text NOT NULL,
 repository text NOT NULL CHECK (repository<>'' AND repository=lower(repository)),
 pull_number bigint NOT NULL CHECK (pull_number>0),
 work_item_id text NOT NULL,
 requested_version text NOT NULL CHECK (requested_version<>''),
 requested_head_sha text NOT NULL CHECK (requested_head_sha<>''),
 request_count bigint NOT NULL CHECK (request_count>0),
 requested_at timestamptz NOT NULL,
 next_attempt_at timestamptz NOT NULL,
 pending boolean NOT NULL,
 attempts bigint NOT NULL CHECK (attempts>=0),
 observed_head_sha text,
 source_updated_at timestamptz,
 observed_at timestamptz,
 state text CHECK (state IN ('open','closed','merged')),
 draft boolean,
 mergeable boolean,
 checks_status text,
 checks_conclusion text,
 checks_total bigint CHECK (checks_total>=0),
 checks_pending bigint CHECK (checks_pending>=0),
 checks_passed bigint CHECK (checks_passed>=0),
 checks_failed bigint CHECK (checks_failed>=0),
 reviews_state text,
 reviews_decision text,
 reviews_approvals bigint CHECK (reviews_approvals>=0),
 reviews_changes_requested bigint CHECK (reviews_changes_requested>=0),
 reviews_comments bigint CHECK (reviews_comments>=0),
 PRIMARY KEY (organization_id,project_id,change_id,repository,pull_number),
 CHECK (
  (observed_at IS NULL AND observed_head_sha IS NULL AND source_updated_at IS NULL
   AND state IS NULL AND draft IS NULL AND mergeable IS NULL
   AND checks_status IS NULL AND checks_conclusion IS NULL AND checks_total IS NULL
   AND checks_pending IS NULL AND checks_passed IS NULL AND checks_failed IS NULL
   AND reviews_state IS NULL AND reviews_decision IS NULL AND reviews_approvals IS NULL
   AND reviews_changes_requested IS NULL AND reviews_comments IS NULL)
  OR
  (observed_at IS NOT NULL AND observed_head_sha IS NOT NULL AND observed_head_sha<>''
   AND source_updated_at IS NOT NULL AND state IS NOT NULL AND draft IS NOT NULL
   AND checks_status IS NOT NULL AND checks_conclusion IS NOT NULL AND checks_total IS NOT NULL
   AND checks_pending IS NOT NULL AND checks_passed IS NOT NULL AND checks_failed IS NOT NULL
   AND reviews_state IS NOT NULL AND reviews_decision IS NOT NULL AND reviews_approvals IS NOT NULL
   AND reviews_changes_requested IS NOT NULL AND reviews_comments IS NOT NULL)
 )
);
CREATE INDEX github_pull_request_refresh_due ON github_pull_request_refresh
 (organization_id,next_attempt_at,project_id,change_id,repository,pull_number) WHERE pending;
ALTER TABLE github_pull_request_refresh ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_pull_request_refresh FORCE ROW LEVEL SECURITY;
GRANT SELECT,INSERT,UPDATE,DELETE ON github_pull_request_refresh TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON github_pull_request_refresh TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON github_pull_request_refresh TO detent_platform USING(true) WITH CHECK(true);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE github_pull_request_refresh;
