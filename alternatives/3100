-- +goose Up
CREATE TABLE workspace_sessions (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 id text NOT NULL CHECK (btrim(id)<>''),
 subject_work_item_id text NOT NULL,
 attempt_id text NOT NULL DEFAULT '',
 ref text NOT NULL CHECK (btrim(ref)<>''),
 head_sha text NOT NULL DEFAULT '',
 runner_id text NOT NULL DEFAULT '',
 machine_id text NOT NULL DEFAULT '',
 lease_id text NOT NULL DEFAULT '',
 fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token>=0),
 state text NOT NULL CHECK (state IN ('requested','starting','ready','idle','unreachable','closing','closed','failed')),
 reason text NOT NULL DEFAULT '',
 requires text[] NOT NULL,
 capabilities text[],
 isolation text NOT NULL DEFAULT '' CHECK (isolation IN ('','user','container')),
 worktree text NOT NULL DEFAULT '' CHECK (worktree IN ('','retained','fresh')),
 machine_hostname text NOT NULL DEFAULT '',
 worktree_path text NOT NULL DEFAULT '',
 read_only boolean NOT NULL DEFAULT false,
 idle_timeout_seconds integer NOT NULL CHECK (idle_timeout_seconds>0),
 expires_at timestamptz NOT NULL,
 requested_expires_at timestamptz NOT NULL,
 opened_at timestamptz,
 last_activity_at timestamptz,
 last_heartbeat_at timestamptz,
 rebind_deadline timestamptz,
 created_by text NOT NULL CHECK (btrim(created_by)<>''),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,id),
 UNIQUE (organization_id,id),
 FOREIGN KEY (organization_id,project_id,subject_work_item_id) REFERENCES issues(organization_id,project_id,native_id),
 CHECK (expires_at>created_at AND requested_expires_at>created_at),
 CHECK ((runner_id='' AND machine_id='' AND lease_id='' AND fencing_token=0) OR (runner_id<>'' AND machine_id<>'' AND lease_id<>'' AND fencing_token>0))
);
CREATE INDEX workspace_subject ON workspace_sessions(organization_id,project_id,subject_work_item_id,created_at,id);
CREATE INDEX workspace_expiry ON workspace_sessions(organization_id,expires_at,id) WHERE state NOT IN ('closed','failed');
CREATE INDEX workspace_actor ON workspace_sessions(organization_id,created_by,state,id);
CREATE UNIQUE INDEX workspace_open_attempt ON workspace_sessions(organization_id,project_id,attempt_id) WHERE attempt_id<>'' AND state NOT IN ('closed','failed');
CREATE TABLE workspace_occupancy (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 workspace_id text NOT NULL,
 runner_id text NOT NULL,
 started_at timestamptz NOT NULL,
 ended_at timestamptz,
 PRIMARY KEY (organization_id,workspace_id,runner_id,started_at),
 FOREIGN KEY (organization_id,project_id,workspace_id) REFERENCES workspace_sessions(organization_id,project_id,id),
 FOREIGN KEY (organization_id,runner_id) REFERENCES runner_identities(organization_id,runner_id),
 CHECK (ended_at IS NULL OR ended_at>=started_at)
);
CREATE INDEX workspace_occupancy_open ON workspace_occupancy(organization_id,project_id,workspace_id) WHERE ended_at IS NULL;
ALTER TABLE workspace_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_sessions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON workspace_sessions FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON workspace_sessions TO detent_runtime;
CREATE POLICY tenant_isolation ON workspace_sessions TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE workspace_occupancy ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_occupancy FORCE ROW LEVEL SECURITY;
REVOKE ALL ON workspace_occupancy FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON workspace_occupancy TO detent_runtime;
CREATE POLICY tenant_isolation ON workspace_occupancy TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE workspace_occupancy;
DROP TABLE workspace_sessions;
