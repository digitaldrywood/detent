-- +goose NO TRANSACTION
-- +goose Up
CREATE UNIQUE INDEX CONCURRENTLY runner_sessions_project_identity ON runner_sessions(organization_id,project_id,id);
CREATE TABLE runner_process_receipts (
 organization_id text NOT NULL,project_id text NOT NULL,session_id bigint NOT NULL,role text NOT NULL CHECK(role IN ('agent','host_validation')),
 runner_id text NOT NULL,enrollment_id text NOT NULL,machine_id text NOT NULL,lease_id text NOT NULL,fencing_token bigint NOT NULL CHECK(fencing_token>0),
 owner_generation bigint NOT NULL DEFAULT 1 CHECK(owner_generation>0),authority_hash text NOT NULL CHECK(length(authority_hash)=64),pid integer NOT NULL CHECK(pid>0),group_id integer NOT NULL CHECK(group_id>=0),started_at timestamptz NOT NULL,started_at_ns bigint NOT NULL CHECK(started_at_ns>0),
 cleanup_root text NOT NULL,cleanup_path text NOT NULL,history_revision bigint NOT NULL,
 registration_object_key text NOT NULL,registration_sha256 text NOT NULL CHECK(length(registration_sha256)=64),registration_bytes bigint NOT NULL CHECK(registration_bytes>0),registration_content_bytes bigint NOT NULL CHECK(registration_content_bytes>0),
 reap_class text NOT NULL DEFAULT 'registered' CHECK(reap_class IN ('registered','rejected','uncertain','committed')),
 reaped_at timestamptz,reap_outcome text NOT NULL DEFAULT '',reap_reason text NOT NULL DEFAULT '',reap_object_key text,reap_sha256 text NOT NULL DEFAULT '',reap_bytes bigint NOT NULL DEFAULT 0,reap_content_bytes bigint NOT NULL DEFAULT 0,updated_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,project_id,session_id,role),
 FOREIGN KEY(organization_id,project_id,session_id) REFERENCES runner_sessions(organization_id,project_id,id),
 FOREIGN KEY(organization_id,session_id,history_revision) REFERENCES runner_session_history(organization_id,session_id,revision),
 FOREIGN KEY(organization_id,runner_id) REFERENCES runner_identities(organization_id,runner_id),
 FOREIGN KEY(organization_id,enrollment_id) REFERENCES runner_enrollments(organization_id,id),
 FOREIGN KEY(organization_id,machine_id) REFERENCES machines(organization_id,id),
 FOREIGN KEY(organization_id,lease_id) REFERENCES leases(organization_id,lease_id),
 CHECK((reap_class='committed')=(reaped_at IS NOT NULL))
);
CREATE INDEX runner_process_active_owner ON runner_process_receipts(organization_id,runner_id,enrollment_id,project_id,session_id,role) WHERE reaped_at IS NULL;

ALTER TABLE runner_process_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_process_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON runner_process_receipts FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON runner_process_receipts TO detent_runtime;
CREATE POLICY tenant_isolation ON runner_process_receipts TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE runner_process_receipts;
DROP INDEX CONCURRENTLY runner_sessions_project_identity;
