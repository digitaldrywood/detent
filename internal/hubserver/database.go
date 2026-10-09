package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const hubApplicationID = 0x44544842

// readerConnections sizes the WAL read pool. Readers never block the
// writer, so the pool is sized for slow list reads and streams to coexist
// with the short reads every runner request makes.
const readerConnections = 32

// authConnections is a separate read pool for credential and scope lookups,
// so authentication never queues behind long reads on the shared pool.
const authConnections = 4

// claimReaderConnections bounds claim preflights on their own read-only pool,
// so a burst of claims never drains the readers that authentication and
// ordinary reads share.
const claimReaderConnections = 2

type database struct {
	linkedSourceBase       string
	db                     *sql.DB
	reader                 *sql.DB
	authReader             *sql.DB
	claimReader            *sql.DB
	lock                   *instancelock.Lock
	path                   string
	schemaVersion          int64
	hostedOrganization     tracker.OrganizationID
	hostedPlans            *HostedPlansConfig
	hostedBilling          bool
	aiCreditMode           string
	aiCreditCostMultiplier float64
	// workspaceRetainAfterRun is workspaces.retain_after_run, read by the
	// claim gate: inside that window an attempt's worktree still exists on
	// the runner that produced it, and only that runner may serve a
	// workspace on the attempt (decisions section 18.1).
	workspaceRetainAfterRun time.Duration
	// workspaceTerminalIsolation is workspaces.terminal.isolation, read by the
	// claim gate so a runner whose terminal is less confined than the
	// organization allows is never handed a workspace that requires one.
	workspaceTerminalIsolation string
	now                        func() time.Time
	newLeaseID                 func() string
	closeOnce                  sync.Once
	closeErr                   error
}

func openDatabase(ctx context.Context, cfg Config) (*database, error) {
	path, err := canonicalDatabasePath(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := cfg.validateDatabaseFilesystem(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("validate hub database filesystem: %w", err)
	}

	lock, err := acquireDatabaseLock(ctx, cfg, path+".lock")
	if err != nil {
		return nil, fmt.Errorf("acquire hub database ownership: %w", err)
	}
	if err := createPrivateDatabaseFile(path); err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	db, err := openTimedWriter(sqliteWriterDSN(path, cfg.BusyTimeout, cfg.Hosted != nil))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open hub database: %w", err), lock.Close())
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &database{db: db, lock: lock, path: path, now: cfg.now, newLeaseID: cfg.newLeaseID, aiCreditCostMultiplier: defaultAICreditCostMultiplier}
	if cfg.Hosted != nil {
		store.linkedSourceBase = cfg.Hosted.PublicURL
		if cfg.Hosted.SharedEntry != nil {
			store.linkedSourceBase += "/organizations/" + cfg.Hosted.OrganizationID
		}
	}
	if cfg.Workspace != nil {
		store.workspaceRetainAfterRun = cfg.Workspace.RetainAfterRun
		store.workspaceTerminalIsolation = cfg.Workspace.Terminal.Isolation
	}
	if err := store.configure(ctx, cfg.BusyTimeout, cfg.Hosted != nil); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.verifyIdentity(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.enableWAL(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.openReader(ctx, cfg.BusyTimeout); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	version, err := runMigrations(ctx, db, cfg.Logger)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	store.schemaVersion = version
	if cfg.CredentialMaintenance {
		var bound int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_tenant").Scan(&bound); err != nil || bound != 1 {
			return nil, errors.Join(ErrHostedDatabaseBinding, store.Close())
		}
	}
	if cfg.hostedBindingMigration {
		return store, nil
	}
	if err := store.bindHostedDatabase(ctx, cfg.Hosted); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if cfg.CredentialMaintenance {
		return store, nil
	}
	if err := store.configureHostedPlans(ctx, cfg.Hosted); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.configureHostedBilling(ctx, cfg.Hosted); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.health(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func acquireDatabaseLock(ctx context.Context, cfg Config, path string) (*instancelock.Lock, error) {
	wait := cfg.Hosted != nil && cfg.Hosted.SharedEntry != nil
	logged := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := instancelock.Acquire(path)
		var held *instancelock.HeldError
		if !wait || !errors.As(err, &held) || held.Owner.PID == os.Getpid() {
			return lock, err
		}
		if holder, inspectErr := process.NewProcessWithContext(ctx, int32(held.Owner.PID)); inspectErr == nil {
			if parent, parentErr := holder.PpidWithContext(ctx); parentErr == nil && parent > 1 && int(parent) == os.Getppid() {
				return nil, err
			}
		}
		if !logged {
			cfg.Logger.InfoContext(ctx, "waiting for previous tenant Hub database ownership", "path", path, "owner_pid", held.Owner.PID)
			logged = true
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func canonicalDatabasePath(rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", errors.New("hub database path is required")
	}
	if path == ":memory:" || strings.HasPrefix(strings.ToLower(path), "file:") {
		return "", errors.New("hub database must be a local filesystem path")
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve hub database path: %w", err)
	}
	parent := filepath.Dir(absolutePath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create hub database directory: %w", err)
	}

	if _, err := os.Lstat(absolutePath); err == nil {
		resolvedPath, resolveErr := filepath.EvalSymlinks(absolutePath)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve hub database symlink: %w", resolveErr)
		}
		return resolvedPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect hub database path: %w", err)
	}

	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve hub database directory: %w", err)
	}
	return filepath.Join(resolvedParent, filepath.Base(absolutePath)), nil
}

func createPrivateDatabaseFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create private hub database: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close new hub database: %w", err)
	}
	return nil
}

func sqliteFileURL(path string) *url.URL {
	databasePath := filepath.ToSlash(path)
	if isWindowsDrivePath(databasePath) {
		databasePath = "/" + databasePath
	}
	return &url.URL{Scheme: "file", Path: databasePath}
}

func sqliteDSN(path string, busyTimeout time.Duration) string {
	return sqliteDSNWithSynchronous(path, busyTimeout, "FULL")
}

func sqliteDSNWithSynchronous(path string, busyTimeout time.Duration, synchronous string) string {
	databaseURL := sqliteFileURL(path)
	query := databaseURL.Query()
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis(busyTimeout)))
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous("+synchronous+")")
	query.Add("_txlock", "immediate")
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String()
}

// sqliteWriterDSN leaves WAL checkpoints to Litestream on hosted tenants:
// Litestream owns checkpointing for every replicated tenant database, and an
// application checkpoint inside a committing request both races it and stalls
// that request. Hosted tenants commit with synchronous=NORMAL: in WAL mode a
// power loss can drop the most recent commits but never corrupts the database,
// and Litestream already bounds hosted durability to its replication lag.
func sqliteWriterDSN(path string, busyTimeout time.Duration, hosted bool) string {
	if !hosted {
		return sqliteDSN(path, busyTimeout)
	}
	dsn := sqliteDSNWithSynchronous(path, busyTimeout, "NORMAL")
	return dsn + "&" + url.Values{"_pragma": {"wal_autocheckpoint(0)"}}.Encode()
}

func writerSynchronousMode(hosted bool) (int, string) {
	if hosted {
		return 1, "NORMAL"
	}
	return 2, "FULL"
}

func sqliteReaderDSN(path string, busyTimeout time.Duration) string {
	databaseURL := sqliteFileURL(path)
	query := databaseURL.Query()
	query.Add("mode", "ro")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis(busyTimeout)))
	query.Add("_pragma", "query_only(1)")
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String()
}

func (d *database) openReader(ctx context.Context, busyTimeout time.Duration) error {
	reader, err := sql.Open("sqlite", sqliteReaderDSN(d.path, busyTimeout))
	if err != nil {
		return fmt.Errorf("open hub database reader: %w", err)
	}
	reader.SetMaxOpenConns(readerConnections)
	reader.SetMaxIdleConns(readerConnections)
	d.reader = reader
	if err := reader.PingContext(ctx); err != nil {
		return fmt.Errorf("open hub database reader: %w", err)
	}
	authReader, err := sql.Open("sqlite", sqliteReaderDSN(d.path, busyTimeout))
	if err != nil {
		return fmt.Errorf("open hub database authentication reader: %w", err)
	}
	authReader.SetMaxOpenConns(authConnections)
	authReader.SetMaxIdleConns(authConnections)
	d.authReader = authReader
	if err := authReader.PingContext(ctx); err != nil {
		return fmt.Errorf("open hub database authentication reader: %w", err)
	}
	claimReader, err := sql.Open("sqlite", sqliteReaderDSN(d.path, busyTimeout))
	if err != nil {
		return fmt.Errorf("open hub claim reader: %w", err)
	}
	claimReader.SetMaxOpenConns(claimReaderConnections)
	claimReader.SetMaxIdleConns(claimReaderConnections)
	d.claimReader = claimReader
	if err := claimReader.PingContext(ctx); err != nil {
		return fmt.Errorf("open hub claim reader: %w", err)
	}
	return nil
}

// auth returns the pool reserved for credential and scope lookups.
func (d *database) auth() *sql.DB {
	if d.authReader != nil {
		return d.authReader
	}
	return d.reader
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 3 || path[1] != ':' || path[2] != '/' {
		return false
	}
	return path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z'
}

func (d *database) configure(ctx context.Context, busyTimeout time.Duration, hosted bool) error {
	wantBusyTimeout := busyTimeoutMillis(busyTimeout)
	var gotBusyTimeout int64
	if err := d.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&gotBusyTimeout); err != nil {
		return fmt.Errorf("read hub sqlite busy timeout: %w", err)
	}
	if gotBusyTimeout != wantBusyTimeout {
		return fmt.Errorf("hub sqlite busy timeout is %dms, want %dms", gotBusyTimeout, wantBusyTimeout)
	}
	var synchronous int
	if err := d.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		return fmt.Errorf("read hub sqlite synchronous mode: %w", err)
	}
	if want, name := writerSynchronousMode(hosted); synchronous != want {
		return fmt.Errorf("hub sqlite synchronous mode is %d, want %s", synchronous, name)
	}

	var foreignKeys int
	if err := d.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read hub sqlite foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("hub sqlite foreign keys are disabled")
	}

	var lockingMode string
	if err := d.db.QueryRowContext(ctx, "PRAGMA locking_mode").Scan(&lockingMode); err != nil {
		return fmt.Errorf("read hub sqlite locking mode: %w", err)
	}
	if !strings.EqualFold(lockingMode, "normal") {
		return fmt.Errorf("hub sqlite locking mode is %q, want normal so live replication can read it", lockingMode)
	}
	if _, err := d.db.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return fmt.Errorf("acquire hub sqlite ownership: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit hub sqlite ownership probe: %w", err)
	}

	return nil
}

func (d *database) enableWAL(ctx context.Context) error {
	var journalMode string
	if err := d.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable hub sqlite WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("hub sqlite journal mode is %q, want wal", journalMode)
	}
	return nil
}

func (d *database) verifyIdentity(ctx context.Context) error {
	var applicationID int64
	if err := d.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return fmt.Errorf("read hub sqlite application id: %w", err)
	}
	if applicationID == hubApplicationID {
		return nil
	}
	if applicationID != 0 {
		return fmt.Errorf("%w: application id is %d", ErrDatabaseIdentity, applicationID)
	}

	var tableCount int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('_litestream_seq', '_litestream_lock')").Scan(&tableCount); err != nil {
		return fmt.Errorf("inspect hub sqlite schema: %w", err)
	}
	if tableCount != 0 {
		return fmt.Errorf("%w: unrecognized tables already exist", ErrDatabaseIdentity)
	}

	if _, err := d.db.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		return fmt.Errorf("set hub sqlite application id: %w", err)
	}
	if err := d.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return fmt.Errorf("verify hub sqlite application id: %w", err)
	}
	if applicationID != hubApplicationID {
		return fmt.Errorf("set hub sqlite application id: got %d", applicationID)
	}
	return nil
}

func (d *database) health(ctx context.Context) error {
	if err := d.reader.PingContext(ctx); err != nil {
		return fmt.Errorf("ping hub database: %w", err)
	}
	version, err := currentSchemaVersion(ctx, d.reader)
	if err != nil {
		return err
	}
	if version != d.schemaVersion {
		return fmt.Errorf("hub database schema version is %d, want %d", version, d.schemaVersion)
	}
	return nil
}

func (d *database) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		var readerErr, authErr, claimReaderErr error
		if d.reader != nil && d.reader != d.db {
			readerErr = d.reader.Close()
		}
		if d.authReader != nil && d.authReader != d.db && d.authReader != d.reader {
			authErr = d.authReader.Close()
		}
		if d.claimReader != nil {
			claimReaderErr = d.claimReader.Close()
		}
		d.closeErr = errors.Join(readerErr, authErr, claimReaderErr, d.db.Close(), d.lock.Close())
	})
	return d.closeErr
}

func busyTimeoutMillis(timeout time.Duration) int64 {
	if timeout <= 0 {
		return defaultBusyTimeout.Milliseconds()
	}
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		return 1
	}
	return milliseconds
}
