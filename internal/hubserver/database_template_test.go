package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var hubDatabaseTemplate struct {
	once sync.Once
	dir  string
	path string
	err  error
}

var hostedSecurityDatabaseTemplate struct {
	once     sync.Once
	contents []byte
	err      error
}

func seedHostedSecurityDatabaseTemplate(t *testing.T, cfg Config) {
	t.Helper()
	hostedSecurityDatabaseTemplate.once.Do(func() {
		path := filepath.Join(t.TempDir(), "security-template.db")
		seedHubDatabaseTemplate(t, path)
		templateCfg := cfg
		templateCfg.DatabasePath = path
		templateCfg.TenantRepository = nil
		templateCfg.AccessRepository = nil
		templateCfg.NativeRunEvents = nil
		ctx := context.Background()
		store, err := openDatabase(ctx, templateCfg.normalized())
		if err != nil {
			hostedSecurityDatabaseTemplate.err = err
			return
		}
		err = seedHostedSecurityProject(ctx, store.db, cfg.Hosted.OrganizationID)
		hostedSecurityDatabaseTemplate.err = errors.Join(err, store.Close())
		if hostedSecurityDatabaseTemplate.err == nil {
			hostedSecurityDatabaseTemplate.contents, hostedSecurityDatabaseTemplate.err = os.ReadFile(path)
		}
	})
	if hostedSecurityDatabaseTemplate.err != nil {
		t.Fatalf("build hosted security template: %v", hostedSecurityDatabaseTemplate.err)
	}
	if err := os.WriteFile(cfg.DatabasePath, hostedSecurityDatabaseTemplate.contents, 0o600); err != nil {
		t.Fatalf("seed hosted security database: %v", err)
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	if err := registerNativeSearchText(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := postgresStartupTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if hubDatabaseTemplate.dir != "" {
		_ = os.RemoveAll(hubDatabaseTemplate.dir)
	}
	cancel()
	os.Exit(code)
}

func seedHubDatabaseTemplate(t *testing.T, path string) {
	t.Helper()
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	hubDatabaseTemplate.once.Do(buildHubDatabaseTemplate)
	if hubDatabaseTemplate.err != nil {
		t.Fatalf("build hub database template: %v", hubDatabaseTemplate.err)
	}
	template := hubDatabaseTemplate.path
	data, err := os.ReadFile(template)
	if err != nil {
		t.Fatalf("read hub database template: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed hub database: %v", err)
	}
}

func buildHubDatabaseTemplate() {
	dir, err := os.MkdirTemp("", "hub-database-template-")
	if err != nil {
		hubDatabaseTemplate.err = err
		return
	}
	hubDatabaseTemplate.dir = dir
	path := filepath.Join(dir, "hub.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if err != nil {
		hubDatabaseTemplate.err = fmt.Errorf("open template: %w", err)
		return
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx := context.Background()
	store := &database{db: db, path: path}
	err = errors.Join(
		store.initializeAutoVacuum(ctx),
		store.configure(ctx, defaultBusyTimeout, false),
		store.verifyIdentity(ctx),
		store.enableWAL(ctx),
	)
	if err == nil {
		_, err = runMigrations(ctx, db, discardLogger())
	}
	hubDatabaseTemplate.err = errors.Join(err, db.Close())
	if hubDatabaseTemplate.err == nil {
		hubDatabaseTemplate.path = path
	}
}

const defaultHubOperatorID = "tok_test_default"

func nativeFixtureStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

func newDefaultNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join(t.TempDir(), "hub.db")
	}
	return newPostgresDefaultNativeFixture(t, cfg)
}

func TestNativeDatabaseTemplateIsolation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first copy", "second copy"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "template-isolation")
			if issue.Number != 1 {
				t.Fatalf("fresh fixture issue number = %d, want 1", issue.Number)
			}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessCredential(t.Context(), credential.ID, f.service.config.now())
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base, f.token, nil), http.StatusUnauthorized)
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "additional")
			if other.project.ID == f.project.ID || other.token == f.token {
				t.Fatal("named fixture reused the default project or token")
			}
			other.create(t, "template-isolation")
		})
	}
}
