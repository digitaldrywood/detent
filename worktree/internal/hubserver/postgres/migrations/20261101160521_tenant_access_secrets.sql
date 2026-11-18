-- +goose Up

CREATE TABLE organizations (
    organization_id text NOT NULL,
    name text NOT NULL,
    slug text NOT NULL DEFAULT '',
    local boolean NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id)
);

CREATE TABLE hosted_tenant (
    organization_id text NOT NULL,
    provider_id text NOT NULL,
    bootstrap_subject text NOT NULL,
    deployment text NOT NULL,
    public_url text NOT NULL,
    allocation_generation bigint NOT NULL,
    PRIMARY KEY (organization_id)
);

CREATE TABLE api_tokens (
    organization_id text NOT NULL,
    id text NOT NULL,
    name text NOT NULL,
    token_hash text NOT NULL,
    token_fingerprint text NOT NULL,
    scope text NOT NULL CHECK (scope IN ('worker','operator','admin')),
    native_only boolean NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    rotated_at timestamptz,
    revoked_at timestamptz,
    expires_at timestamptz,
    last_used_at timestamptz,
    hosted_user_id text NOT NULL DEFAULT '',
    hosted_membership_id text NOT NULL DEFAULT '',
    operator_key_scope text NOT NULL DEFAULT '',
    operator_project_access text NOT NULL DEFAULT 'selected' CHECK (operator_project_access IN ('all','selected')),
    key_kind text NOT NULL DEFAULT 'service' CHECK (key_kind IN ('personal','service')),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, token_hash),
    UNIQUE (organization_id, name)
);

CREATE TABLE hosted_members (
    organization_id text NOT NULL,
    user_id text NOT NULL,
    email text NOT NULL,
    membership_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner','admin','member','reporter','viewer')),
    active boolean NOT NULL,
    principal_id text NOT NULL,
    notify_issues boolean NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, user_id),
    FOREIGN KEY (organization_id, principal_id) REFERENCES api_tokens (organization_id, id)
);

CREATE TABLE hosted_project_grants (
    organization_id text NOT NULL,
    user_id text NOT NULL,
    project_id text NOT NULL,
    can_write boolean NOT NULL,
    manage_runner boolean NOT NULL,
    PRIMARY KEY (organization_id, user_id, project_id),
    FOREIGN KEY (organization_id, user_id) REFERENCES hosted_members (organization_id, user_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE token_grants (
    organization_id text NOT NULL,
    token_id text NOT NULL,
    project_id text NOT NULL,
    PRIMARY KEY (organization_id, token_id, project_id),
    FOREIGN KEY (organization_id, token_id) REFERENCES api_tokens (organization_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE hosted_sessions (
    organization_id text NOT NULL,
    token_hash text NOT NULL,
    email text NOT NULL,
    identity_subject text NOT NULL,
    provider_organization_id text NOT NULL,
    session_id text NOT NULL,
    identity_created_at timestamptz NOT NULL,
    identity_expires_at timestamptz NOT NULL,
    support_actor text NOT NULL,
    support_reason text NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    PRIMARY KEY (organization_id, token_hash)
);

CREATE TABLE hosted_transactions (
    organization_id text NOT NULL,
    token_hash text NOT NULL,
    state text NOT NULL,
    verifier text NOT NULL,
    provider_organization_id text NOT NULL,
    support_actor text NOT NULL,
    support_session text NOT NULL,
    invitation_token text NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    PRIMARY KEY (organization_id, token_hash)
);

CREATE TABLE hosted_invitations (
    organization_id text NOT NULL,
    id text NOT NULL,
    email text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner','admin','member','reporter','viewer')),
    accepted_user_id text NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE hosted_invitation_grants (
    organization_id text NOT NULL,
    invitation_id text NOT NULL,
    project_id text NOT NULL,
    can_write boolean NOT NULL,
    manage_runner boolean NOT NULL,
    PRIMARY KEY (organization_id, invitation_id, project_id),
    FOREIGN KEY (organization_id, invitation_id) REFERENCES hosted_invitations (organization_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE hosted_member_reservations (
    organization_id text NOT NULL,
    email text NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, email)
);

CREATE TABLE hosted_binding_migrations (
    organization_id text NOT NULL,
    id bigint NOT NULL,
    from_public_url text NOT NULL,
    to_public_url text NOT NULL,
    from_deployment text NOT NULL,
    from_generation bigint NOT NULL,
    to_generation bigint NOT NULL,
    migrated_at timestamptz NOT NULL,
    applied boolean NOT NULL,
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE organization_secrets (
    organization_id text NOT NULL,
    kind text NOT NULL,
    organization_slug text NOT NULL,
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    wrapped_data_key bytea NOT NULL,
    master_key_version integer NOT NULL CHECK (master_key_version > 0),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, kind)
);

CREATE TABLE organization_secret_audit (
    organization_id text NOT NULL,
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    actor text NOT NULL,
    kind text NOT NULL,
    key_version integer NOT NULL,
    event text NOT NULL,
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE project_secrets (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    kind text NOT NULL,
    organization_slug text NOT NULL,
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    wrapped_data_key bytea NOT NULL,
    master_key_version integer NOT NULL CHECK (master_key_version > 0),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, kind),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE project_secret_audit (
    organization_id text NOT NULL,
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    project_id text NOT NULL,
    actor text NOT NULL,
    kind text NOT NULL,
    key_version integer NOT NULL,
    event text NOT NULL,
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE hosted_audit (
    organization_id text NOT NULL,
    id bigint GENERATED BY DEFAULT AS IDENTITY,
    session_id text NOT NULL,
    actual_actor text NOT NULL,
    effective_user text NOT NULL,
    reason text NOT NULL,
    event text NOT NULL,
    route text NOT NULL,
    project_id text NOT NULL,
    status integer NOT NULL,
    started_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL,
    recorded_at_ns bigint NOT NULL,
    mutation_object_key text,
    mutation_sha256 text,
    mutation_bytes bigint,
    mutation_content_bytes bigint,
    mutation_format_version integer,
    key_id text NOT NULL DEFAULT '',
    key_kind text NOT NULL DEFAULT '',
    key_owner text NOT NULL DEFAULT '',
    resource text NOT NULL DEFAULT '',
    CHECK ((mutation_object_key IS NULL AND mutation_sha256 IS NULL AND mutation_bytes IS NULL AND mutation_content_bytes IS NULL AND mutation_format_version IS NULL) OR (mutation_object_key IS NOT NULL AND mutation_sha256 IS NOT NULL AND mutation_bytes>0 AND mutation_content_bytes>=0 AND mutation_format_version>0)),
    account_kind text NOT NULL,
    account_retry text NOT NULL,
    PRIMARY KEY (organization_id, id)
);
CREATE TABLE hosted_audit_times (
    organization_id text NOT NULL,
    audit_id bigint NOT NULL,
    recorded_at_ns bigint NOT NULL,
    PRIMARY KEY (organization_id, audit_id),
    FOREIGN KEY (organization_id, audit_id) REFERENCES hosted_audit (organization_id, id)
);

ALTER TABLE organizations OWNER TO detent_migration;

REVOKE ALL ON organizations FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON organizations TO detent_runtime;

ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;

ALTER TABLE organizations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON organizations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_tenant OWNER TO detent_migration;

REVOKE ALL ON hosted_tenant FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_tenant TO detent_runtime;

ALTER TABLE hosted_tenant ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_tenant FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_tenant TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE api_tokens OWNER TO detent_migration;

REVOKE ALL ON api_tokens FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON api_tokens TO detent_runtime;

ALTER TABLE api_tokens ENABLE ROW LEVEL SECURITY;

ALTER TABLE api_tokens FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON api_tokens TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_members OWNER TO detent_migration;

REVOKE ALL ON hosted_members FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_members TO detent_runtime;

ALTER TABLE hosted_members ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_members FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_members TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_project_grants OWNER TO detent_migration;

REVOKE ALL ON hosted_project_grants FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_project_grants TO detent_runtime;

ALTER TABLE hosted_project_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_project_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_project_grants TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE token_grants OWNER TO detent_migration;

REVOKE ALL ON token_grants FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON token_grants TO detent_runtime;

ALTER TABLE token_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE token_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON token_grants TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_sessions OWNER TO detent_migration;

REVOKE ALL ON hosted_sessions FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_sessions TO detent_runtime;

ALTER TABLE hosted_sessions ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_sessions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_sessions TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_transactions OWNER TO detent_migration;

REVOKE ALL ON hosted_transactions FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_transactions TO detent_runtime;

ALTER TABLE hosted_transactions ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_transactions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_transactions TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_invitations OWNER TO detent_migration;

REVOKE ALL ON hosted_invitations FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_invitations TO detent_runtime;

ALTER TABLE hosted_invitations ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_invitations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_invitations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_invitation_grants OWNER TO detent_migration;

REVOKE ALL ON hosted_invitation_grants FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_invitation_grants TO detent_runtime;

ALTER TABLE hosted_invitation_grants ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_invitation_grants FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_invitation_grants TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_member_reservations OWNER TO detent_migration;

REVOKE ALL ON hosted_member_reservations FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_member_reservations TO detent_runtime;

ALTER TABLE hosted_member_reservations ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_member_reservations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_member_reservations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_binding_migrations OWNER TO detent_migration;

REVOKE ALL ON hosted_binding_migrations FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_binding_migrations TO detent_runtime;

ALTER TABLE hosted_binding_migrations ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_binding_migrations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_binding_migrations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE organization_secrets OWNER TO detent_migration;

REVOKE ALL ON organization_secrets FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON organization_secrets TO detent_runtime;

ALTER TABLE organization_secrets ENABLE ROW LEVEL SECURITY;

ALTER TABLE organization_secrets FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON organization_secrets TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE organization_secret_audit OWNER TO detent_migration;

REVOKE ALL ON organization_secret_audit FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON organization_secret_audit TO detent_runtime;

ALTER TABLE organization_secret_audit ENABLE ROW LEVEL SECURITY;

ALTER TABLE organization_secret_audit FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON organization_secret_audit TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE project_secrets OWNER TO detent_migration;

REVOKE ALL ON project_secrets FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON project_secrets TO detent_runtime;

ALTER TABLE project_secrets ENABLE ROW LEVEL SECURITY;

ALTER TABLE project_secrets FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON project_secrets TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE project_secret_audit OWNER TO detent_migration;

REVOKE ALL ON project_secret_audit FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON project_secret_audit TO detent_runtime;

ALTER TABLE project_secret_audit ENABLE ROW LEVEL SECURITY;

ALTER TABLE project_secret_audit FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON project_secret_audit TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_audit OWNER TO detent_migration;

REVOKE ALL ON hosted_audit FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_audit TO detent_runtime;

ALTER TABLE hosted_audit ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_audit FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_audit TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE hosted_audit_times OWNER TO detent_migration;

REVOKE ALL ON hosted_audit_times FROM PUBLIC, detent_platform;

GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_audit_times TO detent_runtime;

ALTER TABLE hosted_audit_times ENABLE ROW LEVEL SECURITY;

ALTER TABLE hosted_audit_times FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON hosted_audit_times TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

GRANT USAGE ON SEQUENCE organization_secret_audit_id_seq, project_secret_audit_id_seq, hosted_audit_id_seq TO detent_runtime;

CREATE INDEX hosted_members_email_idx ON hosted_members (organization_id, email);

CREATE INDEX api_tokens_membership_idx ON api_tokens (organization_id, hosted_user_id, hosted_membership_id);

CREATE INDEX api_tokens_member_page_idx ON api_tokens (organization_id,hosted_user_id,id);

-- +goose Down

DROP TABLE hosted_audit_times;

DROP TABLE hosted_audit;

DROP TABLE project_secret_audit;

DROP TABLE project_secrets;

DROP TABLE organization_secret_audit;

DROP TABLE organization_secrets;

DROP TABLE hosted_binding_migrations;

DROP TABLE hosted_member_reservations;

DROP TABLE hosted_invitation_grants;

DROP TABLE hosted_invitations;

DROP TABLE hosted_transactions;

DROP TABLE hosted_sessions;

DROP TABLE token_grants;

DROP TABLE hosted_project_grants;

DROP TABLE hosted_members;

DROP TABLE api_tokens;

DROP TABLE hosted_tenant;

DROP TABLE organizations;
