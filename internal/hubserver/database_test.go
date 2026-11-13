package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	testTimestamp     = "2026-09-01T12:00:00Z"
	testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"
)

func TestOpenTestServiceUsesPostgres(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(fmt.Sprintf("hosted=%t", hosted), func(t *testing.T) {
			cfg := Config{InitialAdminToken: []byte("example_fixture_admin_token")}
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
			if service.database.postgres == nil || service.config.TenantRepository == nil {
				t.Fatal("required PostgreSQL fixture owners are not configured")
			}
			if hosted && service.config.Hosted != cfg.Hosted {
				t.Fatal("shared service fixture replaced the supplied hosted configuration")
			}
			response := performHubAPIRequest(t, service, http.MethodGet, "/health", string(cfg.InitialAdminToken), nil)
			requireNativeStatus(t, response, http.StatusOK)
			if err := service.retainHostedOperationRecords(t.Context(), time.Now().UTC()); err != nil {
				t.Fatalf("retain hosted operation records: %v", err)
			}
		})
	}
}

var testChangeObjects sync.Map

func openTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	if cfg.Content == nil && cfg.RunDetails == nil && (cfg.Hosted == nil || cfg.Hosted.SharedEntry == nil || cfg.Hosted.SharedEntry.Content == nil && cfg.Hosted.SharedEntry.RunDetails == nil) {
		value, loaded := testChangeObjects.LoadOrStore(t.Name(), &changeObjectFixture{})
		if !loaded {
			t.Cleanup(func() { testChangeObjects.Delete(t.Name()) })
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

			cfg.Postgres = defaults.Postgres
			if cfg.Hosted == nil {
				cfg.Hosted = defaults.Hosted
				seedSuppliedRepositoryBinding(t, repository, cfg.Hosted, cfg.normalized().now().UTC())
			}
		}
	}
	var fixtureAdmin *sql.DB
	if cfg.Postgres == nil {
		admin := postgresStartupTemplate.Database(t)
		fixtureAdmin = admin
		defaults := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
		cfg.DatabaseBackend = DatabasePostgres

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
			deployment, generation := cfg.Hosted.deployment()
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
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

func seedSuppliedRepositoryBinding(t *testing.T, repository *postgres.Repository, hosted *HostedConfig, now time.Time) {
	t.Helper()
	organization := tracker.OrganizationID(hosted.OrganizationID)
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.AccessTenant(t.Context()); !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if _, err := w.AccessOrganization(t.Context()); errors.Is(err, corestore.ErrNotFound) {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Fixture binding", Slug: strings.ReplaceAll(string(organization), "_", "-"), CreatedAt: now.Truncate(time.Microsecond)}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		deployment, generation := hosted.deployment()
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: hosted.WorkOSOrganizationID, BootstrapSubject: hosted.BootstrapSubject, Deployment: deployment, PublicURL: hosted.PublicURL, AllocationGeneration: generation})
	})
	if err != nil {
		t.Fatalf("seed supplied PostgreSQL fixture binding: %v", err)
	}
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
	service.registerSpritePoolRoutes(service.echo)
	service.registerRunnerRoutes(service.echo)
	service.registerCostUsageRoutes(service.echo)
	service.registerOperatorTools(service.echo)
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
	primary := tracker.OrganizationID("org_first")
	for _, organization := range []tracker.OrganizationID{primary, primary + "_second"} {
		seedPostgresHubAdmin(t, service.config.TenantRepository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: newNativeID("hub"), CursorKey: bytes.Repeat([]byte{0x5a}, 32)})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.readNativeCursorKey(t.Context(), organization); err != nil {
			t.Fatal(err)
		}
	}
	service.database.hostedOrganization = primary
	service.localOrganization = string(primary)
	service.requestMetrics.Organization = primary
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
	service.requestMetrics.Organization = previous.requestMetrics.Organization
	return service
}

func authorizeHubTestRequest(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
		issue, err := f.service.readWorkItem(ctx, scope, item)
		if err == nil && string(issue.WorkItemID) != item {
			return fmt.Errorf("work item detail = %s, want %s", issue.WorkItemID, item)
		}
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
		page, err := f.service.readIssues(ctx, scope, url.Values{"limit": {"2"}})
		if err == nil && (page.Total != -1 || len(page.Items) != 2) {
			err = fmt.Errorf("board total = %d, items = %d, want -1 and 2", page.Total, len(page.Items))
		}
		return err
	}}

	tests := []struct {
		name  string
		write bool
	}{{"writer holds a write transaction", true}, {"reader pool holds a long read", false}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newPostgresDefaultNativeFixture(t, Config{})
			issue := f.create(t, "first")
			f.create(t, "second")
			drainTestNativeBoard(t, f.service)
			scope := analyticsTestScope(t, f)
			item := string(issue.WorkItemID)
			installReportsBudgetTrace(t, f.service)
			repository := f.service.config.TenantRepository
			admin := hostedPostgresAdmin(t, f.service)
			poolConfig, err := f.service.config.Postgres.Connection.PoolConfig()
			if err != nil {
				t.Fatal(err)
			}
			login := poolConfig.ConnConfig.User
			heldContext, cancelHeld := context.WithCancel(t.Context())
			ready, release, heldDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var heldErr error
			var secondDone chan struct{}
			var secondErr error
			var cancelSecond context.CancelFunc
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				cancelHeld()
				if cancelSecond != nil {
					cancelSecond()
				}
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
				defer cancel()
				select {
				case <-heldDone:
				case <-cleanup.Done():
					t.Error("held transaction did not stop during failure cleanup")
					return
				}
				if secondDone != nil {
					select {
					case <-secondDone:
					case <-cleanup.Done():
						t.Error("second writer did not stop during failure cleanup")
					}
				}
			})
			hold := func(reader tenantstore.Reader) error {
				if test.write {
					writer := reader.(tenantstore.Writer)
					if err := writer.LockWorkItemProject(heldContext, scope.project); err != nil {
						return err
					}
					record, err := writer.ProjectRecord(heldContext, scope.project)
					if err != nil {
						return err
					}
					record.Core.Name += " held"
					if _, err := writer.UpdateProjectRecord(heldContext, record); err != nil {
						return err
					}
				} else {
					if _, err := reader.WorkItem(heldContext, scope.project, issue.WorkItemID); err != nil {
						return err
					}
				}
				close(ready)
				select {
				case <-release:
					return nil
				case <-heldContext.Done():
					return heldContext.Err()
				}
			}
			go func() {
				defer close(heldDone)
				if test.write {
					heldErr = repository.WriteTenant(heldContext, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error { return hold(writer) })
				} else {
					heldErr = repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(heldContext, scope.organization, corestore.Serving, hold)
				}
			}()
			waitContext := testenv.WaitContext(t, t.Context())
			select {
			case <-ready:
			case <-heldDone:
				t.Fatalf("hold transaction failed: %v", heldErr)
			case <-waitContext.Done():
				t.Fatalf("hold transaction did not become ready: %v", waitContext.Err())
			}
			var heldPID int
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-heldDone:
					t.Fatalf("held transaction ended before reads: %v", heldErr)
				default:
				}
				var matches int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*), coalesce(min(pid),0) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction'", login).Scan(&matches, &heldPID); err != nil {
					return err
				}
				if matches != 1 {
					return fmt.Errorf("held transaction backend matches = %d, want 1", matches)
				}
				return nil
			})
			for _, read := range []read{health, internalHealth, detail, attempts, changes, findings, capabilities, board} {
				started := time.Now()
				if err := read.run(testenv.WaitContext(t, t.Context()), f, scope, item); err != nil {
					t.Fatalf("%s beside a held connection: %v after %s", read.name, err, time.Since(started))
				}
			}
			writerAvailable := false
			readerContext := testenv.WaitContext(t, t.Context())
			err = repository.ReadTenant(readerContext, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				writer, ok := reader.(tenantstore.Writer)
				writerAvailable = ok
				if !ok {
					return nil
				}
				record, err := reader.ProjectRecord(readerContext, scope.project)
				if err != nil {
					return err
				}
				record.Core.Name = "reader write must be rejected"
				_, err = writer.UpdateProjectRecord(readerContext, record)
				return err
			})
			if writerAvailable {
				if pgtransport.SQLState(err) != "25006" {
					t.Fatalf("reader transaction write = %v (SQLSTATE=%s), want read-only rejection", err, pgtransport.SQLState(err))
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !test.write {
				releaseOnce.Do(func() { close(release) })
				select {
				case <-heldDone:
				case <-waitContext.Done():
					t.Fatalf("held read did not complete: %v", waitContext.Err())
				}
				if heldErr != nil {
					t.Fatalf("held read: %v", heldErr)
				}
				if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
					record, err := reader.ProjectRecord(t.Context(), scope.project)
					if err == nil && record.Core.Name != f.project.Name {
						t.Fatalf("rejected reader write changed project: %q", record.Core.Name)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			secondContext, cancel := context.WithCancel(t.Context())
			cancelSecond = cancel
			secondDone = make(chan struct{})
			go func() {
				defer close(secondDone)
				secondErr = repository.WriteTenant(secondContext, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
					if err := writer.LockWorkItemProject(secondContext, scope.project); err != nil {
						return err
					}
					record, err := writer.ProjectRecord(secondContext, scope.project)
					if err != nil {
						return err
					}
					record.Core.Name += " second"
					_, err = writer.UpdateProjectRecord(secondContext, record)
					return err
				})
			}()
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-secondDone:
					t.Fatalf("second writer ended before blocking: %v", secondErr)
				default:
				}
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)))`, login, heldPID).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("second writer has not reached the held project lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			select {
			case <-heldDone:
			case <-waitContext.Done():
				t.Fatalf("held write did not commit: %v", waitContext.Err())
			}
			if heldErr != nil {
				t.Fatalf("held write: %v", heldErr)
			}
			select {
			case <-secondDone:
			case <-waitContext.Done():
				t.Fatalf("second writer did not finish after commit: %v", waitContext.Err())
			}
			if secondErr != nil {
				t.Fatalf("write after held write committed: %v", secondErr)
			}
			if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				record, err := reader.ProjectRecord(t.Context(), scope.project)
				if err == nil && record.Core.Name != f.project.Name+" held second" {
					t.Fatalf("serialized writes did not persist: %q", record.Core.Name)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
