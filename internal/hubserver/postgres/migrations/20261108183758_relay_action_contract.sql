-- +goose Up
CREATE TABLE workspace_relay_tickets (
 organization_id text NOT NULL, project_id text NOT NULL, digest text NOT NULL CHECK (btrim(digest)<>''), workspace_id text NOT NULL,
 session_id text NOT NULL, principal_id text NOT NULL, subject text NOT NULL, support_reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL, redeemed_at timestamptz,
 PRIMARY KEY (organization_id,digest),
 FOREIGN KEY (organization_id,project_id,workspace_id) REFERENCES workspace_sessions(organization_id,project_id,id),
 CHECK (expires_at>created_at)
);
CREATE INDEX relay_ticket_expiry ON workspace_relay_tickets(organization_id,expires_at,digest);
CREATE TABLE workspace_relay_sessions (
 organization_id text NOT NULL, project_id text NOT NULL, workspace_id text NOT NULL, id text NOT NULL,
 principal_id text NOT NULL, subject text NOT NULL, connection_id text NOT NULL, channels text[] NOT NULL,
 opened_at timestamptz NOT NULL, closed_at timestamptz, close_reason text NOT NULL DEFAULT '',
 bytes_in bigint NOT NULL DEFAULT 0 CHECK (bytes_in>=0), bytes_out bigint NOT NULL DEFAULT 0 CHECK (bytes_out>=0),
 recording_artifact text NOT NULL DEFAULT '', support_reason text NOT NULL DEFAULT '',
 PRIMARY KEY (organization_id,project_id,id), UNIQUE (organization_id,connection_id),
 UNIQUE (organization_id,project_id,workspace_id,id),
 FOREIGN KEY (organization_id,project_id,workspace_id) REFERENCES workspace_sessions(organization_id,project_id,id),
 CHECK (closed_at IS NULL OR closed_at>=opened_at)
);
CREATE INDEX relay_workspace ON workspace_relay_sessions(organization_id,project_id,workspace_id,opened_at,id);
CREATE TABLE workspace_terminal_recordings (
 organization_id text NOT NULL, project_id text NOT NULL, workspace_id text NOT NULL, id text NOT NULL,
 relay_session_id text NOT NULL, stream_id text NOT NULL, principal_id text NOT NULL, subject text NOT NULL,
 isolation text NOT NULL CHECK (isolation IN ('user','container')), support_reason text NOT NULL DEFAULT '',
 started_at timestamptz NOT NULL, finished_at timestamptz, terminal_cols integer NOT NULL CHECK (terminal_cols>0),
 terminal_rows integer NOT NULL CHECK (terminal_rows>0), cast_object_key text NOT NULL, cast_sha256 text NOT NULL,
 cast_bytes bigint NOT NULL CHECK (cast_bytes>=0), cast_content_bytes bigint NOT NULL CHECK (cast_content_bytes>=0),
 truncated boolean NOT NULL, revision bigint NOT NULL DEFAULT 1 CHECK (revision>0), updated_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,id), UNIQUE (organization_id,project_id,workspace_id,stream_id),
 FOREIGN KEY (organization_id,project_id,workspace_id,relay_session_id) REFERENCES workspace_relay_sessions(organization_id,project_id,workspace_id,id),
 CHECK (finished_at IS NULL OR finished_at>=started_at)
);
CREATE INDEX recording_workspace ON workspace_terminal_recordings(organization_id,project_id,workspace_id,started_at,id);
CREATE TABLE project_action_runs (
 organization_id text NOT NULL, project_id text NOT NULL, id text NOT NULL, action_id text NOT NULL, workspace_id text NOT NULL,
 command_object_key text NOT NULL, command_sha256 text NOT NULL, command_bytes bigint NOT NULL, command_content_bytes bigint NOT NULL,
 status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed')),
 exit_code integer, reason text NOT NULL DEFAULT '', started_at timestamptz, finished_at timestamptz,
 output_object_key text, output_sha256 text NOT NULL DEFAULT '', output_bytes bigint NOT NULL DEFAULT 0 CHECK (output_bytes>=0),
 output_content_bytes bigint NOT NULL DEFAULT 0 CHECK (output_content_bytes BETWEEN 0 AND 1048576),
 output_sequence bigint NOT NULL DEFAULT 0 CHECK (output_sequence>=0), truncated boolean NOT NULL DEFAULT false,
 claimed_by text NOT NULL DEFAULT '', lease_id text NOT NULL DEFAULT '', fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token>=0),
 created_by text NOT NULL, revision bigint NOT NULL DEFAULT 1 CHECK (revision>0), created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,project_id,id),
 FOREIGN KEY (organization_id,project_id,action_id) REFERENCES project_actions(organization_id,project_id,id),
 FOREIGN KEY (organization_id,project_id,workspace_id) REFERENCES workspace_sessions(organization_id,project_id,id)
);
CREATE INDEX action_run_workspace ON project_action_runs(organization_id,project_id,workspace_id,id);
CREATE INDEX action_run_action ON project_action_runs(organization_id,project_id,action_id,id);

ALTER TABLE workspace_relay_tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_relay_tickets FORCE ROW LEVEL SECURITY;
REVOKE ALL ON workspace_relay_tickets FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON workspace_relay_tickets TO detent_runtime;
CREATE POLICY tenant_isolation ON workspace_relay_tickets TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE workspace_relay_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_relay_sessions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON workspace_relay_sessions FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON workspace_relay_sessions TO detent_runtime;
CREATE POLICY tenant_isolation ON workspace_relay_sessions TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE workspace_terminal_recordings ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_terminal_recordings FORCE ROW LEVEL SECURITY;
REVOKE ALL ON workspace_terminal_recordings FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON workspace_terminal_recordings TO detent_runtime;
CREATE POLICY tenant_isolation ON workspace_terminal_recordings TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE project_action_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_action_runs FORCE ROW LEVEL SECURITY;
REVOKE ALL ON project_action_runs FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON project_action_runs TO detent_runtime;
CREATE POLICY tenant_isolation ON project_action_runs TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE project_action_runs;
DROP TABLE workspace_terminal_recordings;
DROP TABLE workspace_relay_sessions;
DROP TABLE workspace_relay_tickets;
