package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var hubDatabaseTemplate struct {
	once       sync.Once
	dir        string
	path       string
	nativePath string
	err        error
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
	code := m.Run()
	if hubDatabaseTemplate.dir != "" {
		_ = os.RemoveAll(hubDatabaseTemplate.dir)
	}
	os.Exit(code)
}

func seedHubDatabaseTemplate(t *testing.T, path string) {
	t.Helper()
	seedHubDatabaseImage(t, path, false)
}

func seedHubDatabaseImage(t *testing.T, path string, native bool) {
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
	if native {
		template = hubDatabaseTemplate.nativePath
	}
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
		hubDatabaseTemplate.err = buildHubNativeDatabaseTemplate(ctx)
	}
}

const defaultHubProjectID = tracker.ProjectID("prj_00000000000000000000000000003384")
const defaultHubOperatorID = "tok_test_default"
const defaultHubOperatorToken = "hub-test-default-operator-token"

func nativeFixtureStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

func buildHubNativeDatabaseTemplate(ctx context.Context) error {
	data, err := os.ReadFile(hubDatabaseTemplate.path)
	if err != nil {
		return err
	}
	path := filepath.Join(hubDatabaseTemplate.dir, "native.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	store, err := openDatabase(ctx, Config{DatabasePath: path, Logger: discardLogger()}.normalized())
	if err != nil {
		return err
	}
	err = seedHubNativeDefaults(ctx, store)
	err = errors.Join(err, store.Close())
	if err == nil {
		hubDatabaseTemplate.nativePath = path
	}
	return err
}

func seedHubNativeDefaults(ctx context.Context, store *database) error {
	if err := store.ensureInitialAdminToken(ctx, []byte(testHubAdminToken)); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var organization tracker.OrganizationID
	if err := tx.QueryRowContext(ctx, "SELECT id FROM organizations WHERE local = 1").Scan(&organization); err != nil {
		return err
	}
	states := nativeFixtureStates()
	raw, err := json.Marshal(states)
	if err != nil {
		return err
	}
	now := formatHubTime(time.Now())
	if _, err := tx.ExecContext(ctx, "INSERT INTO projects (id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled) VALUES (?, ?, 'native', 'native', ?, 1, ?, 0)", defaultHubProjectID, organization, string(raw), now); err != nil {
		return err
	}
	for _, state := range states {
		if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", defaultHubProjectID, state.Name, state.Name, state.Terminal, state.Dispatchable, now, now); err != nil {
			return err
		}
	}
	hash := apikey.HashToken(defaultHubOperatorToken)
	if _, err := tx.ExecContext(ctx, "INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, native_only, created_at, updated_at) VALUES (?, 'operator-native', ?, ?, 'operator', 1, ?, ?)", defaultHubOperatorID, hash, tokenFingerprint(hash), now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants (token_id, organization_id, project_id) VALUES (?, ?, ?)", defaultHubOperatorID, organization, defaultHubProjectID); err != nil {
		return err
	}
	return tx.Commit()
}

// newDefaultNativeFixture copies immutable defaults into this test's own file.
// Tests needing named projects or additional organizations use newNativeFixture.
func newDefaultNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	started := time.Now()
	defer func() { t.Logf("hub_fixture_default_seconds=%.6f", time.Since(started).Seconds()) }()
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join(t.TempDir(), "hub.db")
	}
	seedHubDatabaseImage(t, cfg.DatabasePath, true)
	service := openTestService(t, cfg)
	var organization tracker.OrganizationID
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local = 1").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	project, err := readNativeProject(t.Context(), service.database.db, nativeScope{organization: organization, project: defaultHubProjectID})
	if err != nil {
		t.Fatal(err)
	}
	return nativeFixture{service: service, project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID), token: defaultHubOperatorToken}
}

func TestNativeDatabaseTemplateIsolation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first copy", "second copy"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			// Identical mutations must succeed independently in both copies.
			issue := f.create(t, "template-isolation")
			if issue.Number != 1 {
				t.Fatalf("fresh fixture issue number = %d, want 1", issue.Number)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, "/api/v1/tokens/"+defaultHubOperatorID, testHubAdminToken, nil), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base, f.token, nil), http.StatusUnauthorized)
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "additional")
			if other.project.ID == f.project.ID || other.token == f.token {
				t.Fatal("named fixture reused the default project or token")
			}
			other.create(t, "template-isolation")
		})
	}
}
