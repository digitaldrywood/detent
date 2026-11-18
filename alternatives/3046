-- +goose Up
CREATE TABLE operator_chat_sessions (
 organization_id text NOT NULL REFERENCES organizations(organization_id),
 connection_id text NOT NULL CHECK (length(connection_id) BETWEEN 1 AND 256),
 principal_id text NOT NULL CHECK (principal_id<>''),
 credential_id text NOT NULL CHECK (credential_id<>''),
 session_id text NOT NULL,
 client text NOT NULL,
 origin_key_id text NOT NULL,
 origin_binding text NOT NULL,
 origin_subject text NOT NULL,
 origin_issuer text NOT NULL,
 origin_generation bigint NOT NULL,
 last_used_at timestamptz NOT NULL,
 PRIMARY KEY (organization_id,connection_id)
);
CREATE INDEX operator_chat_sessions_recent ON operator_chat_sessions(organization_id,last_used_at DESC,connection_id DESC);
ALTER TABLE operator_chat_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE operator_chat_sessions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON operator_chat_sessions FROM PUBLIC;
GRANT SELECT,INSERT,UPDATE,DELETE ON operator_chat_sessions TO detent_runtime;
CREATE POLICY tenant_isolation ON operator_chat_sessions TO detent_runtime,detent_migration
 USING (organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE operator_chat_sessions;
