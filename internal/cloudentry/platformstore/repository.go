package platformstore

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/schemafingerprint"
)

var (
	ErrPlatformRole         = errors.New("PostgreSQL platform role requires separate platform authority")
	ErrInvalid              = errors.New("invalid platform storage record")
	ErrNoSession            = errors.New("shared session is missing, expired or revoked")
	ErrOrganizationConflict = errors.New("organization conflicts with an existing registry record")
)

type Config struct {
	Pool       PoolConfig
	SigningKey ed25519.PrivateKey
	Keys       *hubsecrets.Keyring
	Now        func() time.Time
}

type PoolConfig struct {
	MaxConnections        int
	ServingConnections    int
	BackgroundConnections int
}

type Repository struct {
	pool       *sql.DB
	serving    chan struct{}
	background chan struct{}
	secrets    *secretCipher
	now        func() time.Time
}

func New(ctx context.Context, pool *sql.DB, config Config) (*Repository, error) {
	limits := config.Pool
	if pool == nil || limits.MaxConnections < 3 || limits.ServingConnections < 1 || limits.BackgroundConnections < 1 || limits.ServingConnections >= limits.MaxConnections || limits.BackgroundConnections > limits.MaxConnections-limits.ServingConnections || limits.BackgroundConnections > limits.MaxConnections-2 {
		return nil, ErrInvalid
	}
	serving := min(limits.ServingConnections, limits.MaxConnections-limits.BackgroundConnections-1)
	secrets, err := newSecretCipher(config.SigningKey, config.Keys)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(limits.MaxConnections)
	pool.SetMaxIdleConns(limits.MaxConnections)
	var allowed bool
	err = pool.QueryRowContext(ctx, `
		SELECT pg_has_role(current_user,$1::text,'USAGE')
		AND NOT pg_has_role(session_user,$2::text,'MEMBER')
		AND NOT pg_has_role(session_user,$3::text,'MEMBER')
 AND NOT EXISTS (
 SELECT 1 FROM pg_roles r WHERE (r.rolname=current_user OR pg_has_role(session_user,r.oid,'MEMBER'))
 AND (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication))
		AND EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname='platform' AND pg_get_userbyid(n.nspowner)=$3::text)
 AND (SELECT count(*) FROM pg_class c WHERE c.relnamespace=(SELECT oid FROM pg_namespace WHERE nspname='platform')
 AND c.relname IN ('organizations','organization_events','organization_intents','organization_slugs','organization_slug_counters','billing_customers','billing_events','entitlement_changes','platform_members','platform_member_changes','platform_notifications','archive_storage_bindings','archive_deletion_receipts','archive_deletion_failures','sessions','authorizations','transactions','audit','administration_receipts','access_keys','organization_key_policy','access_key_organizations','access_key_notifications','access_key_projects','access_key_blocked_projects','github_user_connections','oauth_clients','oauth_client_redirect_uris','oauth_grants','oauth_refresh_tokens','oauth_requests','oauth_spent_refresh_tokens','auth_notifications') AND c.relkind IN ('r','p')
 AND has_table_privilege(current_user,c.oid,'SELECT')
 AND has_table_privilege(current_user,c.oid,'INSERT')
 AND has_table_privilege(current_user,c.oid,'UPDATE')
 AND has_table_privilege(current_user,c.oid,'DELETE'))=33
		AND NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='platform' AND pg_has_role(session_user,c.relowner,'MEMBER'))`, postgres.PlatformRoleName, postgres.RuntimeRoleName, postgres.MigrationRoleName).Scan(&allowed)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if !allowed {
		return nil, ErrPlatformRole
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Repository{pool: pool, serving: make(chan struct{}, serving), background: make(chan struct{}, limits.BackgroundConnections), secrets: secrets, now: config.Now}, nil
}

func (r *Repository) Close() error { return r.pool.Close() }

func (r *Repository) Ping(ctx context.Context) error { return mapError(ctx, r.pool.PingContext(ctx)) }

func (r *Repository) SchemaVersion(ctx context.Context) (int64, error) {
	var version int64
	if err := r.pool.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM (
		SELECT version_id, is_applied, ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY id DESC) AS state_rank
		FROM public.goose_db_version
	) AS migration_state WHERE state_rank = 1 AND is_applied`).Scan(&version); err != nil {
		return 0, mapError(ctx, err)
	}
	return version, nil
}

func (r *Repository) SchemaFingerprint(ctx context.Context) (string, error) {
	rows, err := r.pool.QueryContext(ctx, `SELECT version_id FROM (
		SELECT version_id, is_applied, ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY id DESC) AS state_rank
		FROM public.goose_db_version WHERE version_id > 0
	) AS migration_state WHERE state_rank = 1 AND is_applied ORDER BY version_id`)
	if err != nil {
		return "", mapError(ctx, err)
	}
	defer rows.Close()
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return "", err
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return schemafingerprint.Versions(versions), nil
}

func (r *Repository) transaction(ctx context.Context, workload corestore.Workload, readOnly bool, run func(*sql.Tx) error) (resultErr error) {
	tx, err := r.BeginEntry(ctx, workload, readOnly)
	if err != nil {
		return err
	}
	defer tx.cleanup(&resultErr)
	if err := run(tx.Tx); err != nil {
		return mapError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mapError(ctx, tx.Commit())
}

func mapError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && (errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return ctx.Err()
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		var domain error
		switch state.SQLState() {
		case "23505":
			domain = corestore.ErrUniqueViolation
		case "40001", "40P01":
			domain = corestore.ErrSerializationFailure
		case "57014":
			domain = ctx.Err()
		}
		return &storageError{cause: err, domain: domain, state: state.SQLState()}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &storageError{cause: err, domain: corestore.ErrNotFound}
	}
	return err
}

type storageError struct {
	cause  error
	domain error
	state  string
}

func (e *storageError) Error() string {
	if e.domain != nil {
		return e.domain.Error()
	}
	return "PostgreSQL platform operation failed with SQLSTATE " + e.state
}
func (e *storageError) Unwrap() error        { return e.cause }
func (e *storageError) Is(target error) bool { return errors.Is(e.domain, target) }
