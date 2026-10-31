package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
			SELECT pg_has_role(current_user, 'detent_runtime', 'USAGE')
			AND NOT EXISTS (
				SELECT 1 FROM pg_roles r
				WHERE (r.rolname = current_user OR pg_has_role(session_user, r.oid, 'MEMBER'))
				AND (r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication)
			)
			AND NOT pg_has_role(session_user, 'detent_platform', 'MEMBER')
			AND NOT pg_has_role(session_user, 'detent_migration', 'MEMBER')
			AND NOT EXISTS (
				SELECT 1 FROM pg_class c
				WHERE c.oid IN (
				'public.projects'::regclass, 'public.issues'::regclass,
				'public.native_comments'::regclass,
				'public.request_metric_windows'::regclass,
				'public.request_metric_callers'::regclass,
				'public.request_minutes'::regclass,
				'public.request_hours'::regclass, 'public.request_days'::regclass
				)
				AND (pg_has_role(session_user, c.relowner, 'MEMBER') OR NOT c.relrowsecurity OR NOT c.relforcerowsecurity)
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
	tx, err := pool.BeginTx(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("begin PostgreSQL tenant transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('detent.organization_id', $1, true)`, organization); err != nil {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		return nil, errors.Join(fmt.Errorf("bind PostgreSQL tenant: %w", err), rollbackErr)
	}
	return tx, nil
}
