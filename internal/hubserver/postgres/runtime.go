package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrTenantRequired = errors.New("authenticated organization is required")
var ErrRuntimeRole = errors.New("PostgreSQL runtime role is not tenant restricted")

type Runtime struct {
	reader *sql.DB
	writer *sql.DB
}

func NewRuntime(ctx context.Context, reader, writer *sql.DB) (*Runtime, error) {
	for _, pool := range []*sql.DB{reader, writer} {
		if pool == nil {
			return nil, ErrRuntimeRole
		}
		var allowed bool
		err := pool.QueryRowContext(ctx, `
			SELECT current_setting('server_version_num')::integer / 10000 IN (17, 18)
			AND current_setting('row_security') = 'on'
			AND COALESCE(current_setting('detent.organization_id', true), '') = ''
			AND current_schemas(false) IN (ARRAY['public']::name[], ARRAY['public', 'pg_catalog']::name[])
			AND pg_has_role(current_user, 'detent_runtime_v2', 'USAGE')
			AND has_schema_privilege(current_user, 'public', 'USAGE')
			AND NOT has_schema_privilege(current_user, 'public', 'CREATE')
			AND NOT has_schema_privilege(current_user, 'pg_catalog', 'CREATE')
			AND NOT has_schema_privilege(current_user, 'platform', 'USAGE')
			AND NOT has_schema_privilege(current_user, 'platform', 'CREATE')
			AND NOT has_database_privilege(current_user, current_database(), 'CREATE')
			AND has_function_privilege(current_user, 'public.allocate_core_record_id(regclass,bigint)', 'EXECUTE')
			AND has_function_privilege(current_user, 'public.convert_history_payload(text,text,text,text,bigint,text,bigint,text,text,bigint,bigint,text,text,bigint,bigint)', 'EXECUTE')
			AND NOT EXISTS (
				SELECT 1 FROM pg_roles r
				WHERE (r.rolname = current_user OR pg_has_role(session_user, r.oid, 'MEMBER'))
				AND (r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication)
			)
			AND NOT pg_has_role(session_user, 'detent_platform_v2', 'MEMBER')
			AND NOT pg_has_role(session_user, 'detent_migration_v2', 'MEMBER')
			AND EXISTS (
				SELECT 1 FROM pg_proc p
				WHERE p.oid = 'public.search_index_candidates(tsquery,text,text[],boolean,text,text,text,text,text,boolean,text,timestamptz,text,bigint,text[],boolean,text[],text[],double precision[],text[],bigint[],boolean[])'::regprocedure
				AND p.proowner = 'detent_platform_v2'::regrole AND p.prosecdef AND p.provolatile = 's'
				AND p.proconfig = ARRAY['search_path=pg_catalog']
				AND has_function_privilege(current_user, p.oid, 'EXECUTE')
				AND NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a
					WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')
			)
			AND NOT EXISTS (
				SELECT 1 FROM pg_class c
				WHERE c.oid IN (
				'public.organizations'::regclass, 'public.hosted_tenant'::regclass, 'public.api_tokens'::regclass, 'public.token_grants'::regclass, 'public.hosted_members'::regclass, 'public.hosted_project_grants'::regclass, 'public.hosted_sessions'::regclass, 'public.hosted_transactions'::regclass, 'public.hosted_invitations'::regclass, 'public.hosted_invitation_grants'::regclass, 'public.hosted_member_reservations'::regclass, 'public.hosted_binding_migrations'::regclass, 'public.hosted_audit'::regclass, 'public.hosted_audit_times'::regclass, 'public.organization_secrets'::regclass, 'public.project_secrets'::regclass, 'public.organization_secret_audit'::regclass, 'public.project_secret_audit'::regclass,
 'public.projects'::regclass, 'public.project_settings'::regclass, 'public.project_workflow_states'::regclass, 'public.project_workflow_transitions'::regclass, 'public.project_slugs'::regclass, 'public.issues'::regclass,
				'public.native_comments'::regclass,
				'public.search_documents'::regclass,
				'public.collaboration_events'::regclass,
				'public.native_commands'::regclass,
				'public.collaboration_versions'::regclass,
				'public.change_requests'::regclass,
				'public.change_issue_links'::regclass,
				'public.change_versions'::regclass,
				'public.change_evidence'::regclass,
				'public.storage_conversion_progress'::regclass,
				'public.storage_conversion_cursors'::regclass,
				'public.request_metric_windows'::regclass,
				'public.request_metric_imports'::regclass,
				'public.access_imports'::regclass,
				'public.request_metric_callers'::regclass,
				'public.request_minutes'::regclass,
				'public.machines'::regclass, 'public.leases'::regclass,
				'public.native_attempts'::regclass, 'public.native_attempt_events'::regclass,
				'public.native_attempt_runs'::regclass,
				'public.request_hours'::regclass, 'public.request_days'::regclass
				)
				AND (pg_has_role(session_user, c.relowner, 'MEMBER') OR NOT c.relrowsecurity OR NOT c.relforcerowsecurity
					OR NOT has_table_privilege(current_user, c.oid, 'SELECT')
					OR has_table_privilege(current_user, c.oid, 'TRUNCATE')
					OR (c.oid <> 'public.storage_conversion_progress'::regclass
						AND (NOT has_table_privilege(current_user, c.oid, 'INSERT')
							OR NOT has_table_privilege(current_user, c.oid, 'UPDATE')))
					OR (c.oid NOT IN ('public.storage_conversion_progress'::regclass, 'public.storage_conversion_cursors'::regclass, 'public.request_metric_imports'::regclass, 'public.access_imports'::regclass)
						AND NOT has_table_privilege(current_user, c.oid, 'DELETE')))
			)
			AND NOT EXISTS (
				SELECT 1 FROM pg_class c
				WHERE c.oid IN ('public.issues_id_seq'::regclass, 'public.native_comments_record_id_seq'::regclass, 'public.leases_fencing_token_seq'::regclass)
				AND (NOT has_sequence_privilege(current_user, c.oid, 'USAGE')
					OR has_sequence_privilege(current_user, c.oid, 'UPDATE'))
			)`).Scan(&allowed)
		if err != nil {
			return nil, fmt.Errorf("validate PostgreSQL runtime role: %w", err)
		}
		if !allowed {
			return nil, ErrRuntimeRole
		}
	}
	return &Runtime{reader: reader, writer: writer}, nil
}

func (r *Runtime) Begin(ctx context.Context, organization tracker.OrganizationID, options *sql.TxOptions) (*sql.Tx, error) {
	if strings.TrimSpace(string(organization)) == "" {
		return nil, ErrTenantRequired
	}
	pool := r.writer
	if options != nil && options.ReadOnly {
		pool = r.reader
	}
	started := time.Now()
	tx, err := pool.BeginTx(ctx, options)
	begin := "BEGIN"
	if options != nil && options.ReadOnly {
		begin = "BEGIN READ ONLY"
	}
	observeStatement(ctx, started, begin)
	if err != nil {
		return nil, fmt.Errorf("begin PostgreSQL tenant transaction: %w", err)
	}
	if _, err := (timedQueries{DBTX: tx}).ExecContext(ctx, `SELECT set_config('search_path', 'public, pg_catalog', true), set_config('detent.organization_id', $1, true)`, organization); err != nil {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		return nil, errors.Join(fmt.Errorf("bind PostgreSQL tenant: %w", err), rollbackErr)
	}
	return tx, nil
}
