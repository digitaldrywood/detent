package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/pressly/goose/v3"
)

func TestRunReportsSlowMigration(t *testing.T) {
	key := hostedSharedKey(17)
	cfg := hostedSharedTestConfig(hostedTestDatabasePath(t), newHostedSecurityProvider(), key, 1)
	cfg.ListenAddress = "127.0.0.1:0"
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	latest := supportedSchemaVersion(t)
	cfg.migrate = func(ctx context.Context, db *sql.DB, logger *slog.Logger) (int64, error) {
		files, err := fs.Sub(migrationFiles, "migrations")
		if err != nil {
			return 0, err
		}
		migrations := append(hubGoMigrations(logger), goose.NewGoMigration(latest+1, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil))
		return runMigrationsFromFS(ctx, db, files, logger, migrations)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listening := make(chan struct{})
	cfg.listen = func(context.Context, string, string) (net.Listener, error) { close(listening); return listener, nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		unblock()
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-started:
	case err := <-done:
		done <- err
		t.Fatalf("startup failed: %v", err)
	case <-t.Context().Done():
		t.Fatal("migration did not start")
	}
	select {
	case <-listening:
	default:
		t.Fatal("health listener is not bound during migrations")
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
			assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: "org_security", Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: "/internal/v1/health", BodyDigest: cloudassert.BodyDigest([]byte("{}")), IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), ID: id})
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
	if status != http.StatusServiceUnavailable || !health.MigrationsRunning || health.SchemaVersion != latest {
		t.Fatalf("during migration = %d %+v", status, health)
	}
	unblock()
	testenv.WaitUntil(t, t.Context(), func() error {
		status, health = probe(true)
		if status != http.StatusOK {
			return fmt.Errorf("status = %d", status)
		}
		return nil
	})
	if health.MigrationsRunning || health.SchemaVersion != latest+1 {
		t.Fatalf("after migration = %+v", health)
	}
}
