package hubserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	testTimestamp     = "2026-09-01T12:00:00Z"
	testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"
)

func TestSQLiteDSNEncodesAbsolutePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantPrefix string
	}{
		{name: "Unix", path: "/var/lib/detent/hub.db", wantPrefix: "file:///var/lib/detent/hub.db?"},
		{name: "Windows uppercase drive", path: "C:/detent/hub.db", wantPrefix: "file:///C:/detent/hub.db?"},
		{name: "Windows lowercase drive", path: "d:/detent/hub.db", wantPrefix: "file:///d:/detent/hub.db?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := sqliteDSN(test.path, defaultBusyTimeout)
			if !strings.HasPrefix(got, test.wantPrefix) {
				t.Fatalf("sqliteDSN() = %q, want prefix %q", got, test.wantPrefix)
			}
		})
	}
}

func TestOpenExcludesAnotherHubProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	for _, test := range []struct {
		name                    string
		shared, cancel, sibling bool
	}{
		{name: "standalone refuses contention"},
		{name: "shared entry waits for previous owner", shared: true},
		{name: "shared entry cancels ownership wait", shared: true, cancel: true},
		{name: "shared entry refuses live sibling", shared: true, sibling: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bound := testenv.WaitContext(t, t.Context())
			path := filepath.Join(t.TempDir(), "hub.db")
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHubOwnerHelperProcess$")
			command.Env = append(os.Environ(), "DETENT_HUB_OWNER_HELPER="+path, "GOCOVERDIR="+t.TempDir())
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				_ = stdin.Close()
				if !waited {
					_ = command.Wait()
				}
			})
			ready, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || strings.TrimSpace(ready) != "ready" {
				t.Fatalf("helper readiness = %q: %v", ready, err)
			}

			if test.sibling {
				contender := exec.CommandContext(bound, os.Args[0], "-test.run=^TestHubOwnerHelperProcess$")
				contender.Env = append(os.Environ(), "DETENT_HUB_OWNER_HELPER="+path, "DETENT_HUB_OWNER_CONTENDER=1", "GOCOVERDIR="+t.TempDir())
				output, err := contender.CombinedOutput()
				if err != nil || !strings.Contains(string(output), "held\n") {
					t.Fatalf("live sibling contender = %v, %s", err, output)
				}
				return
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := Config{DatabasePath: path, Logger: discardLogger()}
			waiting := make(chan struct{}, 1)
			if test.shared {
				cfg = hostedSharedTestConfig(path, newHostedSecurityProvider(), hostedSharedKey(7), 1)
				cfg.Logger = slog.New(databaseLockWaitHandler{Handler: slog.DiscardHandler, waiting: waiting})
			}
			type opened struct {
				service *Service
				err     error
			}
			result := make(chan opened, 1)
			go func() {
				service, err := Open(ctx, cfg)
				result <- opened{service: service, err: err}
			}()
			if test.shared {
				select {
				case <-waiting:
				case got := <-result:
					if got.service != nil {
						_ = got.service.Close()
					}
					t.Fatalf("shared tenant exited instead of waiting: %v", got.err)
				case <-bound.Done():
					t.Fatal("tenant did not wait on previous owner")
				}
				if test.cancel {
					cancel()
				} else {
					_ = stdin.Close()
				}
			}
			var got opened
			select {
			case got = <-result:
			case <-bound.Done():
				t.Fatal("tenant startup did not finish")
			}
			if got.service != nil {
				t.Cleanup(func() { _ = got.service.Close() })
			}
			switch {
			case !test.shared:
				if !errors.Is(got.err, instancelock.ErrHeld) {
					t.Fatalf("Open() = %v, want held", got.err)
				}
			case test.cancel:
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("Open() = %v, want cancellation", got.err)
				}
			default:
				if got.err != nil {
					t.Fatalf("Open() after holder exit = %v", got.err)
				}
				if err := got.service.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Fatal(err)
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("holder exit = %v, %s", err, stderr.String())
			}
			waited = true
			service := openTestService(t, cfg)
			if service.database.path == "" {
				t.Fatal("reopened database path is empty")
			}
			if test.shared {
				_, err := Open(t.Context(), cfg)
				if !errors.Is(err, instancelock.ErrHeld) {
					t.Fatalf("same-process contender = %v, want held", err)
				}
			}
		})
	}
}

type databaseLockWaitHandler struct {
	slog.Handler
	waiting chan struct{}
}

func (h databaseLockWaitHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h databaseLockWaitHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "waiting for previous tenant Hub database ownership" {
		select {
		case h.waiting <- struct{}{}:
		default:
		}
	}
	return h.Handler.Handle(ctx, record)
}

func TestHubOwnerHelperProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	path := os.Getenv("DETENT_HUB_OWNER_HELPER")
	if path == "" {
		return
	}
	if os.Getenv("DETENT_HUB_OWNER_CONTENDER") != "" {
		cfg := hostedSharedTestConfig(path, newHostedSecurityProvider(), hostedSharedKey(7), 1)
		service, err := Open(t.Context(), cfg)
		if service != nil {
			_ = service.Close()
		}
		if !errors.Is(err, instancelock.ErrHeld) {
			t.Fatalf("live sibling contention = %v", err)
		}
		fmt.Fprintln(os.Stdout, "held")
		return
	}

	service, err := Open(t.Context(), Config{DatabasePath: path, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatalf("write readiness: %v", err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for parent: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestOnlineBackupPreservesSchemaAndData(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, _ := seedProjection(t, service.database.db)
	backupPath := filepath.Join(t.TempDir(), "backups", "hub.db")
	if err := service.Backup(t.Context(), backupPath); err != nil {
		t.Fatalf("Backup() error = %v", err)
	}
	if err := service.database.health(t.Context()); err != nil {
		t.Fatalf("source health after backup: %v", err)
	}

	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	t.Cleanup(func() {
		if err := backup.Close(); err != nil {
			t.Fatalf("close backup: %v", err)
		}
	})
	var integrity string
	if err := backup.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("backup integrity check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("backup integrity = %q, want ok", integrity)
	}
	var gotRepositoryID int64
	if err := backup.QueryRowContext(t.Context(), "SELECT id FROM repositories WHERE github_node_id = 'R_repo'").Scan(&gotRepositoryID); err != nil {
		t.Fatalf("query backed up repository: %v", err)
	}
	if gotRepositoryID != repositoryID {
		t.Fatalf("backed up repository id = %d, want %d", gotRepositoryID, repositoryID)
	}
	version, err := currentSchemaVersion(t.Context(), backup)
	if err != nil {
		t.Fatalf("backup schema version: %v", err)
	}
	if version != supportedSchemaVersion(t) {
		t.Fatalf("backup schema version = %d, want %d", version, supportedSchemaVersion(t))
	}
	var applicationID int64
	if err := backup.QueryRowContext(t.Context(), "PRAGMA application_id").Scan(&applicationID); err != nil {
		t.Fatalf("backup application id: %v", err)
	}
	if applicationID != hubApplicationID {
		t.Fatalf("backup application id = %d, want %d", applicationID, hubApplicationID)
	}
}

func TestBackupRejectsUnsafeDestinations(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	if err := service.Backup(t.Context(), service.database.path); !errors.Is(err, ErrBackupSource) {
		t.Fatalf("Backup(source) error = %v, want ErrBackupSource", err)
	}
	existing := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(existing, []byte("preserve"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := service.Backup(t.Context(), existing); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Backup(existing) error = %v, want os.ErrExist", err)
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "preserve" {
		t.Fatalf("existing backup content = %q, want preserve", content)
	}
}

func TestBackupCancellationRemovesPartialDestination(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	destination := filepath.Join(t.TempDir(), "backup.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Backup(ctx, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("Backup() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup destination Stat() error = %v, want os.ErrNotExist", err)
	}
}

func TestOpenTestServiceUsesPostgres(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(fmt.Sprintf("hosted=%t", hosted), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.db")
			cfg := Config{DatabasePath: path, InitialAdminToken: []byte("example_fixture_admin_token")}
			if hosted {
				plans := pilotHostedPlans()
				cfg.Hosted = &HostedConfig{
					Plans:                &plans,
					OrganizationID:       "org_example",
					WorkOSOrganizationID: "provider_example",
					PublicURL:            "https://hub.example.test",
					Provider:             newHostedSecurityProvider(),
				}
			}
			service := openTestService(t, cfg)
			if service.database.postgres == nil || service.database.db != nil || service.database.reader != nil || service.database.authReader != nil || service.database.claimReader != nil {
				t.Fatal("shared service fixture must use PostgreSQL without legacy SQL handles")
			}
			if hosted && service.config.Hosted != cfg.Hosted {
				t.Fatal("shared service fixture replaced the supplied hosted configuration")
			}
			response := performHubAPIRequest(t, service, http.MethodGet, "/health", string(cfg.InitialAdminToken), nil)
			requireNativeStatus(t, response, http.StatusOK)
			if err := service.retainHostedOperationRecords(t.Context(), time.Now().UTC()); err != nil {
				t.Fatalf("retain hosted operation records: %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy database path exists or cannot be checked: %v", err)
			}
		})
	}
}

var testChangeObjects sync.Map

func openTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	if cfg.Content == nil && cfg.RunDetails == nil && (cfg.Hosted == nil || cfg.Hosted.SharedEntry == nil || cfg.Hosted.SharedEntry.Content == nil && cfg.Hosted.SharedEntry.RunDetails == nil) {
		value, loaded := testChangeObjects.LoadOrStore(cfg.DatabasePath, &changeObjectFixture{})
		if !loaded {
			t.Cleanup(func() { testChangeObjects.Delete(cfg.DatabasePath) })
		}
		cfg.Content = value.(*changeObjectFixture).call
	}
	if cfg.RunDetails == nil && (cfg.Hosted == nil || cfg.Hosted.SharedEntry == nil || cfg.Hosted.SharedEntry.RunDetails == nil) {
		cfg.RunDetails = &memoryRunDetails{}
	}
	if cfg.RunnerReleaseClient == nil {
		cfg.RunnerReleaseClient = &runnerReleaseFixture{}
	}
	if cfg.Conversation != nil && cfg.Conversation.Enabled {
		conversationConfig := *cfg.Conversation
		cfg.Conversation = &conversationConfig
		if cfg.Hosted != nil && cfg.Hosted.SharedEntry != nil {
			hosted := *cfg.Hosted
			shared := *hosted.SharedEntry
			hosted.SharedEntry = &shared
			cfg.Hosted = &hosted
			if shared.ArchiveContents == nil && shared.Content == nil {
				shared.ArchiveContents = &conversationContentFixture{}
			}
		} else if conversationConfig.ArchiveContents == nil && cfg.Content == nil {
			conversationConfig.ArchiveContents = &conversationContentFixture{}
		}
	}

	started := time.Now()
	defer func() { t.Logf("hub_fixture_open_seconds=%.6f", time.Since(started).Seconds()) }()
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	if len(cfg.InitialAdminToken) == 0 {
		cfg.InitialAdminToken = []byte(testHubAdminToken)
	}
	if repository, ok := cfg.TenantRepository.(*postgres.Repository); ok && cfg.Postgres == nil {
		if admin, ok := projectRecordDatabases.Load(repository); ok {
			defaults := startupConfig(startupConnection(t, admin.(*sql.DB), "detent_runtime_v2", ""))
			cfg.DatabaseBackend = DatabasePostgres
			cfg.DatabasePath = ""
			cfg.Postgres = defaults.Postgres
			if cfg.Hosted == nil {
				cfg.Hosted = defaults.Hosted
			}
		}
	}
	if cfg.Postgres == nil {
		admin := postgresStartupTemplate.Database(t)
		defaults := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
		cfg.DatabaseBackend = DatabasePostgres
		cfg.DatabasePath = ""
		cfg.Postgres = defaults.Postgres
		if cfg.Hosted == nil {
			cfg.Hosted = defaults.Hosted
		}
		store, err := openDatabase(t.Context(), cfg.normalized())
		if err != nil {
			t.Fatalf("open PostgreSQL fixture: %v", err)
		}
		organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
		now := cfg.normalized().now().UTC()
		err = store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "example", CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, AllocationGeneration: 1}); err != nil {
				return err
			}
			return w.CreateAccessCredential(t.Context(), accessstore.Credential{
				OrganizationID: organization, ID: "test_hub_admin", Name: "Example administrator",
				Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "admin",
				CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service",
			})
		})
		if err := errors.Join(err, store.Close()); err != nil {
			t.Fatalf("seed PostgreSQL fixture: %v", err)
		}
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return service
}

func authorizeHubTestRequest(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
}

func seedProjection(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	repository, err := db.ExecContext(t.Context(), "INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)", "R_repo", "digitaldrywood", "detent", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	repositoryID, err := repository.LastInsertId()
	if err != nil {
		t.Fatalf("repository id: %v", err)
	}
	seedCompatibilityProject(t, db, repositoryID)
	workflow, err := db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, github_node_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)", repositoryID, "WS_todo", "Todo", "Todo", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert workflow state: %v", err)
	}
	workflowID, err := workflow.LastInsertId()
	if err != nil {
		t.Fatalf("workflow state id: %v", err)
	}
	issue, err := db.ExecContext(t.Context(), "INSERT INTO issues (repository_id, workflow_state_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", repositoryID, workflowID, "I_issue", 1, "Issue", "https://example.test/1", "open", "v1", testTimestamp, testTimestamp, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	issueID, err := issue.LastInsertId()
	if err != nil {
		t.Fatalf("issue id: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO pull_requests (repository_id, issue_id, github_node_id, github_number, title, url, github_state, head_ref, head_sha, base_ref, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", repositoryID, issueID, "PR_one", 1, "PR", "https://example.test/pr/1", "open", "feature", "abc", "main", "v1", testTimestamp, testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert pull request: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO github_webhook_inbox (delivery_id, repository_id, event_type, payload_sha256, payload_bytes, status, received_at, last_received_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", "delivery-one", repositoryID, "issues", "abc", 2, "pending", testTimestamp, testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert webhook delivery: %v", err)
	}
	return repositoryID, issueID
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedCompatibilityProject(t *testing.T, db *sql.DB, repositoryID int64) {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='projects')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO projects(organization_id,repository_id,name,profile,created_at)
SELECT o.id,r.id,r.github_owner || '/' || r.github_name,'github_compatible',r.created_at FROM organizations o CROSS JOIN repositories r WHERE o.local=1 AND r.id=?
ON CONFLICT(repository_id) DO NOTHING`, repositoryID); err != nil {
		t.Fatalf("seed compatibility project: %v", err)
	}
}

func TestReaderPoolReadsBesideHeldConnections(t *testing.T) {
	t.Parallel()

	type read struct {
		name string
		run  func(context.Context, nativeFixture, nativeScope, string) error
	}
	health := read{"health", func(ctx context.Context, f nativeFixture, _ nativeScope, _ string) error {
		if response, status := f.service.readInstanceHealth(ctx); status != http.StatusOK {
			return fmt.Errorf("health = %d %q", status, response.Status)
		}
		return nil
	}}
	internalHealth := read{"internal health", func(ctx context.Context, f nativeFixture, _ nativeScope, _ string) error {
		return f.service.database.health(ctx)
	}}
	detail := read{"work item detail", func(ctx context.Context, f nativeFixture, scope nativeScope, item string) error {
		_, _, err := readNativeIssue(ctx, f.service.database.reader, scope, item)
		return err
	}}
	attempts := read{"attempts", func(ctx context.Context, f nativeFixture, scope nativeScope, item string) error {
		_, err := f.service.readAttempts(ctx, scope, item, url.Values{})
		return err
	}}
	changes := read{"changes", func(ctx context.Context, f nativeFixture, scope nativeScope, item string) error {
		_, err := f.service.readChanges(ctx, scope, item)
		return err
	}}
	findings := read{"health findings", func(ctx context.Context, f nativeFixture, scope nativeScope, _ string) error {
		_, err := f.service.readHealthFindings(ctx, scope, "", "", "", 10)
		return err
	}}
	capabilities := read{"capabilities", func(ctx context.Context, f nativeFixture, _ nativeScope, _ string) error {
		_, err := f.service.readNativeCapabilities(ctx)
		return err
	}}
	board := read{"board", func(ctx context.Context, f nativeFixture, scope nativeScope, _ string) error {
		page, err := f.service.readIssues(ctx, scope, url.Values{"limit": {"1"}})
		if err == nil && page.Total != 2 {
			err = fmt.Errorf("board total = %d, want 2", page.Total)
		}
		return err
	}}

	tests := []struct {
		name  string
		hold  func(*testing.T, *Service) *sql.Tx
		reads []read
		write bool
	}{
		{
			name: "writer holds a write transaction",
			hold: func(t *testing.T, s *Service) *sql.Tx {
				tx, err := s.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET name = name"); err != nil {
					t.Fatal(err)
				}
				return tx
			},
			reads: []read{health, internalHealth, detail, attempts, changes, findings, capabilities},
			write: true,
		},
		{
			name: "reader pool holds a long read",
			hold: func(t *testing.T, s *Service) *sql.Tx {
				tx, err := s.database.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				var count int
				if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM issues").Scan(&count); err != nil {
					t.Fatal(err)
				}
				return tx
			},
			reads: []read{health, internalHealth, detail, attempts, changes, findings, capabilities, board},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "first")
			f.create(t, "second")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			item := string(issue.WorkItemID)

			held := test.hold(t, f.service)
			defer func() { _ = held.Rollback() }()
			for _, read := range test.reads {
				started := time.Now()
				err := read.run(testenv.WaitContext(t, t.Context()), f, scope, item)
				if err != nil {
					t.Fatalf("%s beside a held connection: %v after %s", read.name, err, time.Since(started))
				}
			}

			if _, err := f.service.database.reader.ExecContext(t.Context(), "UPDATE projects SET name = name"); err == nil {
				t.Fatal("reader pool accepted a write")
			}
			if !test.write {
				window := operatortool.AnalyticsWindow{From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC().Add(time.Hour), Bucket: time.Hour}
				read := func() int {
					t.Helper()
					analytics, err := readNativeAnalytics(t.Context(), held, scope, operatortool.AnalyticsRequest{Limit: 10}, window)
					if err != nil {
						t.Fatal(err)
					}
					return analytics.LaneResidence.IssuesObserved
				}
				if got := read(); got != 2 {
					t.Fatalf("initial analytics population = %d, want 2", got)
				}
				body, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-third"}, Title: "third", Body: issueContractTestSections, State: "Todo"})
				if err != nil {
					t.Fatal(err)
				}
				ctx := testenv.WaitContext(t, t.Context())
				request := httptest.NewRequest(http.MethodPost, f.base+"/work-items", strings.NewReader(string(body))).WithContext(ctx)
				request.Header.Set("Authorization", "Bearer "+f.token)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				requireNativeStatus(t, response, http.StatusOK)
				if got := read(); got != 2 {
					t.Fatalf("held analytics snapshot after write = %d, want 2", got)
				}
				page, err := f.service.readIssues(ctx, scope, url.Values{})
				if err != nil || page.Total != 3 {
					t.Fatalf("committed population = %d, want 3: %v", page.Total, err)
				}
				return
			}
			blocked, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			if _, err := f.service.database.db.ExecContext(blocked, "UPDATE projects SET name = name"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("second write beside a held write = %v, want it to wait for the writer", err)
			}
			if err := held.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET name = name"); err != nil {
				t.Fatalf("write after the held write committed: %v", err)
			}
		})
	}
}

func TestHostedWriterLeavesCheckpointsToLitestream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		hosted          bool
		want            int
		wantSynchronous int
	}{
		{name: "hosted tenant", hosted: true, want: 0, wantSynchronous: 1},
		{name: "self-managed Hub", hosted: false, want: 1000, wantSynchronous: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", sqliteWriterDSN(filepath.Join(t.TempDir(), "hub.db"), defaultBusyTimeout, test.hosted))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			var got int
			if err := db.QueryRowContext(t.Context(), "PRAGMA wal_autocheckpoint").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("wal_autocheckpoint = %d, want %d", got, test.want)
			}
			var synchronous int
			if err := db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
				t.Fatal(err)
			}
			if synchronous != test.wantSynchronous {
				t.Fatalf("synchronous = %d, want %d", synchronous, test.wantSynchronous)
			}
		})
	}
}
