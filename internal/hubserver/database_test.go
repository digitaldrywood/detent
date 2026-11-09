package hubserver

import (
	"bytes"
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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

const (
	testTimestamp     = "2026-09-01T12:00:00Z"
	testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"
)

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
		objectKey := cfg.DatabasePath
		if objectKey == "" {
			objectKey = t.Name()
		}
		value, loaded := testChangeObjects.LoadOrStore(objectKey, &changeObjectFixture{})
		if !loaded {
			t.Cleanup(func() { testChangeObjects.Delete(objectKey) })
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
	var fixtureAdmin *sql.DB
	if cfg.Postgres == nil {
		admin := postgresStartupTemplate.Database(t)
		fixtureAdmin = admin
		defaults := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
		cfg.DatabaseBackend = DatabasePostgres
		cfg.DatabasePath = ""
		cfg.Postgres = defaults.Postgres
		if cfg.now != nil || cfg.Clock != nil {
			cfg.Postgres.LeaseClock = cfg.normalized().now
		}
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
	if fixtureAdmin != nil {
		projectRecordDatabases.Store(service.database.postgres, fixtureAdmin)
		t.Cleanup(func() { projectRecordDatabases.Delete(service.database.postgres) })
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return service
}

func hubPostgresRepository(t *testing.T, maxConnections int) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Postgres.Pool = postgres.PoolConfig{MaxConnections: maxConnections, ServingConnections: maxConnections - 1, BackgroundConnections: 1}
	store, err := openPostgresDatabase(t.Context(), *cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	repository := store.postgres
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
		projectRecordDatabases.Delete(repository)
	})
	return repository, admin
}

func readPostgresTestRunner(ctx context.Context, service *Service, organization tracker.OrganizationID, runnerID string, now time.Time) (runnerauth.Runner, error) {
	var runner runnerauth.Runner
	err := service.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		routing, ok := reader.(runnerstore.RoutingReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		record, err := routing.RunnerRoutingRecord(ctx, runnerID, now)
		if err != nil {
			return err
		}
		urgent, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		runner = runnerFromRoutingRecord(record, now, urgent)
		return nil
	})
	return runner, err
}

func newClockedPostgresNativeService(t *testing.T, cfg Config) *Service {
	t.Helper()
	if cfg.Hosted != nil {
		return newPostgresNativeService(t, cfg)
	}
	if cfg.SecretKeys == nil {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
	}
	service := openTestService(t, cfg)
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	service.config.Hosted = nil
	service.echo = echo.New()
	service.echo.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware)
	service.registerNativeRoutes(service.echo)
	service.registerRunnerRoutes(service.echo)
	service.registerOperatorTools(service.echo)
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
	primary := tracker.OrganizationID("org_first")
	for _, organization := range []tracker.OrganizationID{primary, primary + "_second"} {
		seedPostgresHubAdmin(t, service.config.TenantRepository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
		key := bytes.Repeat([]byte{0x5a}, 32)
		envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		now := service.config.now().UTC().Truncate(time.Microsecond)
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: newNativeID("hub"), CursorKey: key}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.database.hostedOrganization = primary
	service.localOrganization = string(primary)
	service.requestMetrics.organization = primary
	return service
}

func reopenPostgresNativeService(t *testing.T, previous *Service) *Service {
	t.Helper()
	admin := usagePostgresAdmin(t, previous)
	cfg := previous.config
	store, err := openPostgresDatabase(t.Context(), *cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	repository := store.postgres
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
	cfg.Hosted = startupConfig(cfg.Postgres.Connection).Hosted
	service := openLocalPostgresHubTestService(t, repository, cfg)
	service.database.hostedOrganization = previous.database.hostedOrganization
	service.localOrganization = previous.localOrganization
	service.requestMetrics.organization = previous.requestMetrics.organization
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
