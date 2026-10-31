package platformstore

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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
	if pool == nil || limits.MaxConnections < 2 || limits.ServingConnections < 1 || limits.BackgroundConnections < 1 || limits.ServingConnections >= limits.MaxConnections || limits.BackgroundConnections > limits.MaxConnections-limits.ServingConnections {
		return nil, ErrInvalid
	}
	secrets, err := newSecretCipher(config.SigningKey, config.Keys)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(limits.MaxConnections)
	pool.SetMaxIdleConns(limits.MaxConnections)
	var allowed bool
	err = pool.QueryRowContext(ctx, `
 SELECT pg_has_role(current_user,'detent_platform','USAGE')
 AND NOT pg_has_role(session_user,'detent_runtime','MEMBER')
 AND NOT pg_has_role(session_user,'detent_migration','MEMBER')
 AND NOT EXISTS (
 SELECT 1 FROM pg_roles r WHERE (r.rolname=current_user OR pg_has_role(session_user,r.oid,'MEMBER'))
 AND (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication))
 AND EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname='platform' AND pg_get_userbyid(n.nspowner)='detent_migration')
 AND (SELECT count(*) FROM pg_class c WHERE c.relnamespace=(SELECT oid FROM pg_namespace WHERE nspname='platform')
 AND c.relname IN ('organizations','sessions','authorizations','transactions','access_keys','github_user_connections') AND c.relkind IN ('r','p')
 AND has_table_privilege(current_user,c.oid,'SELECT')
 AND has_table_privilege(current_user,c.oid,'INSERT')
 AND has_table_privilege(current_user,c.oid,'UPDATE')
 AND has_table_privilege(current_user,c.oid,'DELETE'))=6
 AND NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='platform' AND pg_has_role(session_user,c.relowner,'MEMBER'))`).Scan(&allowed)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if !allowed {
		return nil, ErrPlatformRole
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Repository{pool: pool, serving: make(chan struct{}, limits.ServingConnections), background: make(chan struct{}, limits.BackgroundConnections), secrets: secrets, now: config.Now}, nil
}

func (r *Repository) Close() error { return r.pool.Close() }

func (r *Repository) transaction(ctx context.Context, workload corestore.Workload, readOnly bool, run func(*sql.Tx) error) error {
	slots := r.serving
	switch workload {
	case corestore.Serving:
	case corestore.Background:
		slots = r.background
	default:
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slots }()
	tx, err := r.pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: readOnly})
	if err != nil {
		return mapError(ctx, err)
	}
	defer tx.Rollback()
	if err := run(tx); err != nil {
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
