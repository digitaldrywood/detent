package hubserver

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/schemafingerprint"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestRunReportsSchemaValidation(t *testing.T) {
	t.Parallel()
	key := hostedSharedKey(17)
	admin, cfg, expected := postgresStartupHealthFixture(t, func(cfg *Config) {
		cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	})
	cfg.ListenAddress = "127.0.0.1:0"
	lock := lockStartupLedger(t, admin)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	listening := make(chan struct{})
	cfg.deps.Listen = func(context.Context, string, string) (net.Listener, error) { close(listening); return listener, nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		_ = lock.Rollback()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	waitForStartupLedger(t, admin, cfg.Postgres.Connection.User, done)
	select {
	case <-listening:
	default:
		t.Fatal("health listener is not bound during schema validation")
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	probe := func(signed bool) (int, MigrationHealth) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+listener.Addr().String()+"/internal/v1/health", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if signed {
			id, err := cloudassert.NewID()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: "org_startup", Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: "/internal/v1/health", BodyDigest: cloudassert.BodyDigest([]byte("{}")), IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), ID: id})
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(cloudassert.Header, assertion)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var health MigrationHealth
		if signed {
			if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode, health
	}
	if status, _ := probe(false); status != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d", status)
	}
	status, health := probe(true)
	if status != http.StatusServiceUnavailable || !health.MigrationsRunning || health.SchemaFactsComplete || health.SchemaVersion != 0 || health.SchemaFingerprint != "" {
		t.Fatalf("during schema validation = %d %+v", status, health)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		status, health = probe(true)
		if status != http.StatusOK {
			return fmt.Errorf("status = %d", status)
		}
		return nil
	})
	if health.MigrationsRunning || !health.SchemaFactsComplete || health.SchemaVersion != expected.SchemaVersion || health.SchemaFingerprint != expected.SchemaFingerprint {
		t.Fatalf("after schema validation = %+v", health)
	}
}

func TestStandaloneStartupHealthRequiresReadCredential(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted = nil
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	progress := openPostgresStartupHealth(t, cfg)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()
	progress.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated startup health status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "migrations_running") {
		t.Fatalf("unauthenticated startup health disclosed progress: %s", recorder.Body.String())
	}
}

func TestStandaloneStartupHealthReportsAuthenticatedProgress(t *testing.T) {
	t.Parallel()
	_, cfg, expected := postgresStartupHealthFixture(t)
	progress := openPostgresStartupHealth(t, cfg)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
	recorder := httptest.NewRecorder()
	progress.ServeHTTP(recorder, request)
	var health struct {
		MigrationHealth
		Status  string `json:"status"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable || health.Status != "unavailable" || health.MigrationsRunning || health.SchemaFactsComplete ||
		health.SchemaVersion != expected.SchemaVersion || health.SchemaFingerprint != expected.SchemaFingerprint ||
		health.Version != "v1" || health.Commit != "abc123" {
		t.Fatalf("startup health = %d %+v", recorder.Code, health)
	}
}

func TestRunCancelsSchemaValidationWhenListenerFails(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.ListenAddress = "127.0.0.1:0"
	lock := lockStartupLedger(t, admin)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.deps.Listen = func(context.Context, string, string) (net.Listener, error) { return listener, nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		_ = lock.Rollback()
		_ = listener.Close()
		<-done
	})
	waitForStartupLedger(t, admin, cfg.Postgres.Connection.User, done)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Run error = %v, want closed listener", err)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("listener failure did not cancel PostgreSQL schema validation")
	}
	assertStartupDisconnected(t, admin, cfg.Postgres.Connection)
}

func postgresStartupHealthFixture(t *testing.T, configure ...func(*Config)) (*sql.DB, Config, MigrationHealth) {
	t.Helper()
	ctx := t.Context()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Version, cfg.Commit = "v1", "abc123"
	for _, apply := range configure {
		apply(&cfg)
	}
	store, err := openDatabase(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	seedStartupOrganization(t, store.postgres, "org_startup", cfg.Hosted)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := admin.QueryContext(ctx, `SELECT version_id FROM public.goose_db_version WHERE version_id>0 AND is_applied ORDER BY version_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("PostgreSQL fixture has no applied schema versions")
	}
	return admin, cfg, MigrationHealth{SchemaVersion: versions[len(versions)-1], SchemaFingerprint: schemafingerprint.Versions(versions)}
}

func openPostgresStartupHealth(t *testing.T, cfg Config) *startupHealth {
	t.Helper()
	progress := &startupHealth{config: cfg}
	service, err := openService(t.Context(), cfg, progress)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return progress
}

func lockStartupLedger(t *testing.T, admin *sql.DB) *sql.Tx {
	t.Helper()
	admin.SetMaxOpenConns(2)
	lock, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	})
	if _, err := lock.ExecContext(t.Context(), `LOCK TABLE public.goose_db_version IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	return lock
}

func waitForStartupLedger(t *testing.T, admin *sql.DB, user string, done chan error) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("startup stopped before reaching the schema ledger: %v", err)
		default:
		}
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
			WHERE a.usename=$1 AND l.locktype='relation' AND NOT l.granted
			AND l.relation='public.goose_db_version'::regclass
		)`, user).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("startup has not reached the schema ledger lock")
		}
		return nil
	})
}
