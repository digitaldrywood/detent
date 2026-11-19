package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type writerReadGuardKey struct{}

type writerReadGuard struct {
	mu           sync.Mutex
	violations   []string
	queries      atomic.Int64
	transactions atomic.Int64
	readOnly     map[*pgx.Conn]bool
	repository   *postgres.Repository
}

var writerReadAllowlist = map[string]string{
	"recordHostedRequest": "Hosted GET request accounting updates consumption in its own write transaction.",
	"recordAccessAudit":   "Support-session GETs record their access audit in its own write transaction.",
}

func (g *writerReadGuard) check(ctx context.Context, operation string) error {
	if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); !marked {
		return nil
	}
	pcs := make([]uintptr, 40)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	var stack []string
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, "/hubserver.") {
			name := frame.Function[strings.LastIndex(frame.Function, ".")+1:]
			if reason := writerReadAllowlist[name]; reason != "" {
				return nil
			}
			stack = append(stack, name)
		}
		if !more {
			break
		}
	}
	message := operation + ": " + strings.Join(stack, " -> ")
	g.mu.Lock()
	g.violations = append(g.violations, message)
	g.mu.Unlock()
	return errors.New("read-only GET used the writer: " + message)
}

func (g *writerReadGuard) traceStatement(ctx context.Context, conn *pgx.Conn, query string) context.Context {
	statement := strings.ToUpper(strings.TrimSpace(query))
	if strings.HasPrefix(statement, "BEGIN") || strings.HasPrefix(statement, "START TRANSACTION") {
		readOnly := strings.Contains(statement, " READ ONLY")
		g.mu.Lock()
		if g.readOnly == nil {
			g.readOnly = make(map[*pgx.Conn]bool)
		}
		g.readOnly[conn] = readOnly
		g.mu.Unlock()
		if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); marked && readOnly {
			g.transactions.Add(1)
		}
		if !readOnly {
			if err := g.check(ctx, "PostgreSQL read-write transaction"); err != nil {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled
			}
		}
		return ctx
	}
	if statement == "COMMIT" || statement == "ROLLBACK" {
		g.mu.Lock()
		delete(g.readOnly, conn)
		g.mu.Unlock()
		return ctx
	}
	if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); marked {
		g.queries.Add(1)
		g.mu.Lock()
		readOnly := g.readOnly[conn]
		g.mu.Unlock()
		if !readOnly {
			if err := g.check(ctx, "PostgreSQL query outside a read-only transaction"); err != nil {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled
			}
		}
	}
	return ctx
}

func (g *writerReadGuard) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return g.traceStatement(ctx, conn, data.SQL)
}

func (g *writerReadGuard) TraceQueryEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if conn.PgConn().TxStatus() == 'I' {
		g.mu.Lock()
		delete(g.readOnly, conn)
		g.mu.Unlock()
	}
}

func (g *writerReadGuard) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		ctx = g.traceStatement(ctx, conn, query.SQL)
		if ctx.Err() != nil {
			return ctx
		}
	}
	return ctx
}

func (*writerReadGuard) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (g *writerReadGuard) TraceBatchEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	if conn.PgConn().TxStatus() == 'I' {
		g.mu.Lock()
		delete(g.readOnly, conn)
		g.mu.Unlock()
	}
}

func installWriterReadGuard(t *testing.T, s *Service, guard *writerReadGuard) *writerReadGuard {
	t.Helper()
	if guard == nil || guard.repository == nil || s.database.postgres != guard.repository || s.config.TenantRepository != guard.repository || s.config.AccessRepository != guard.repository || s.database.billingRepository != guard.repository || s.config.BillingRepository != guard.repository || s.requestMetrics == nil || s.requestMetrics.Repository != guard.repository {
		t.Fatal("read guard does not cover every actual PostgreSQL owner")
	}
	ctx := context.WithValue(t.Context(), writerReadGuardKey{}, true)
	called := false
	err := s.database.postgres.WriteTenant(ctx, s.database.hostedOrganization, corestore.Serving, func(tenantstore.Writer) error {
		called = true
		return nil
	})
	guard.mu.Lock()
	violations := len(guard.violations)
	guard.violations = nil
	guard.mu.Unlock()
	if err == nil || called || violations == 0 {
		t.Fatalf("guard missed actual PostgreSQL writer: callback=%v violations=%d error=%v", called, violations, err)
	}
	if err := s.database.postgres.ReadTenant(ctx, s.database.hostedOrganization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.AccessOrganization(ctx)
		return err
	}); err != nil {
		t.Fatalf("guard refused actual PostgreSQL reader: %v", err)
	}
	if guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
		t.Fatal("guard did not observe actual PostgreSQL read-only transaction and query")
	}
	guard.queries.Store(0)
	guard.transactions.Store(0)
	s.echo.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), writerReadGuardKey{}, true)))
			}
			return next(c)
		}
	})
	return guard
}

func newWriterGuardNativeFixture(t *testing.T) (nativeFixture, *writerReadGuard) {
	t.Helper()
	guard := &writerReadGuard{}
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, guard)
	})
	guard.repository = repository
	service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	organization := nativeOrganization{ID: "org_first", Name: "Example organization", Slug: "example-first"}
	seedPostgresHubAdmin(t, repository, service, organization)
	key := bytes.Repeat([]byte{0x5a}, 32)
	envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	service.localOrganization = string(organization.ID)
	service.database.hostedOrganization = organization.ID
	service.requestMetrics.Organization = organization.ID
	return newNativeFixture(t, service, organization.ID, "reader-guard"), guard
}

func newWriterGuardHostedFixture(t *testing.T, listen bool, configure ...func(*Config)) (*browserHostedFixture, *writerReadGuard) {
	t.Helper()
	guard := &writerReadGuard{}
	open := func(t *testing.T, cfg Config) *Service {
		t.Helper()
		cfg = hostedPostgresConfig(t, cfg)
		value, ok := projectRecordDatabases.Load(cfg.Postgres.Connection.Database)
		if !ok {
			t.Fatal("guarded hosted PostgreSQL fixture database is unavailable")
		}
		admin := value.(*sql.DB)
		config := postgresStartupTemplate.NativeConfig(t, admin, cfg.Postgres.Connection.User, cfg.Postgres.Connection.Password)
		config.MaxConns = int32(cfg.Postgres.Pool.MaxConnections)
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, guard)
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := postgres.NewRepository(t.Context(), pool, cfg.Postgres.Pool)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repository.Close(); err != nil {
				t.Error(err)
			}
		})
		projectRecordDatabases.Store(repository, admin)
		t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
		guard.repository = repository
		return openPostgresHubTestService(t, repository, cfg)
	}
	fixture := newBrowserHostedFixtureBackendWithService(t, true, "org_browser_preview", listen, open, configure...)
	return fixture, guard
}

func TestReadOnlyGETsNeverAcquireWriter(t *testing.T) {
	t.Parallel()
	t.Run("native", func(t *testing.T) {
		f, guard := newWriterGuardNativeFixture(t)
		issue := f.create(t, "readable issue")
		approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
		runner.enroll(t)
		installWriterReadGuard(t, f.service, guard)
		for _, suffix := range []string{"", "/onboarding", "/policy", "/labels", "/integration", "/actions", "/work-items", "/health/findings", "/runner-diagnostics", "/artifact-services", "/work-items/" + string(issue.WorkItemID), "/work-items/" + string(issue.WorkItemID) + "/comments", "/work-items/" + string(issue.WorkItemID) + "/runtime", "/work-items/" + string(issue.WorkItemID) + "/history", "/work-items/" + string(issue.WorkItemID) + "/attempts", "/work-items/" + string(issue.WorkItemID) + "/changes", "/work-items/" + string(issue.WorkItemID) + "/explanation", "/work-items/" + string(issue.WorkItemID) + "/pull-requests", "/work-items/" + string(issue.WorkItemID) + "/artifacts"} {
			t.Run(suffix, func(t *testing.T) {
				r := performHubAPIRequest(t, f.service, http.MethodGet, f.base+suffix, f.token, nil)
				if r.Code != http.StatusOK {
					t.Errorf("GET %s returned %d: %s", suffix, r.Code, r.Body.String())
				}
			})
		}
		for _, suffix := range []string{"/routing", "/capacity", "/update"} {
			t.Run("runner"+suffix, func(t *testing.T) {
				response := performHubAPIRequest(t, f.service, http.MethodGet, runner.identityPath()+suffix, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
			})
		}
		t.Run("/onboarding warm", func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/onboarding", f.token, map[string]any{"idempotency_key": "reader-guard-progress", "progress": onboarding.Progress{Repository: "existing", Artifacts: "local"}}), http.StatusOK)
			cold := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, cold, http.StatusOK)
			tenant := hubmaintenance.Tenant{Organization: string(f.project.OrganizationID), Generation: 1}
			if _, err := f.service.OnboardingPageMaintenanceFeature().Step((hubmaintenance.Budget{Items: 1, Bytes: integrationPageMaintenanceBytes}).Context(t.Context(), tenant), tenant, hubmaintenance.Budget{}); err != nil {
				t.Fatal(err)
			}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, key, err := onboardingViewer(credential)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(projectstore.OnboardingPageReader).OnboardingPage(t.Context(), f.project.ID, key)
				return err
			}); err != nil {
				t.Fatalf("saved viewer page was not prepared: %v", err)
			}
			warm := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, warm, http.StatusOK)
			if warm.Body.String() != cold.Body.String() {
				t.Fatalf("warm onboarding changed the response:\ncold=%s\nwarm=%s", cold.Body.String(), warm.Body.String())
			}
		})
		f.service.tokenUseWork.Wait()
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if len(guard.violations) > 0 {
			t.Fatalf("writer reads: %s", strings.Join(guard.violations, "\n"))
		}
		if guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
			t.Fatal("GET matrix did not use traced PostgreSQL read-only transactions")
		}
	})
	t.Run("hosted", func(t *testing.T) {
		f, guard := newWriterGuardHostedFixture(t, false, func(cfg *Config) {
			cfg.Hosted.EntitlementAdminToken = []byte(strings.Repeat("e", 32))
			cfg.Hosted.EntitlementAdministrator = "test-operator"
		})
		installWriterReadGuard(t, f.service, guard)
		board := func(project, state string) string {
			return browserHostedOrganizationBase + "/work-board?project=" + project + "&state=" + url.QueryEscape(state)
		}
		active := `{"view":"board","tab":"active","sort":"default","completedWindow":"24h"}`
		refusals := []struct {
			name, account, path string
			status              int
		}{
			{"invalid state", "owner", board(f.project, `{"view":"board","unknown":true}`), http.StatusUnprocessableEntity},
			{"hidden project", "viewer", board(f.privateProject, active), http.StatusNotFound},
			{"revoked session", "revoked", board(f.project, active), http.StatusUnauthorized},
		}
		t.Run("/work-board pending", func(t *testing.T) {
			response := f.page(t, "owner", board(f.project, active))
			requireNativeStatus(t, response, http.StatusOK)
			var view workBoardView
			decodeHubResponse(t, response, &view)
			if len(view.Projects) != 1 || !view.Projects[0].ProjectionPending || view.Projects[0].Work != nil || len(view.Cards) != 0 {
				t.Fatalf("pending work board=%s", response.Body.String())
			}
			for _, refusal := range refusals {
				requireNativeStatus(t, f.page(t, refusal.account, refusal.path), refusal.status)
			}
		})
		drainTestNativeBoard(t, f.service)
		t.Run("/work-board ready", func(t *testing.T) {
			response := f.page(t, "owner", board(f.project, active))
			requireNativeStatus(t, response, http.StatusOK)
			var view workBoardView
			decodeHubResponse(t, response, &view)
			if len(view.Projects) != 1 || string(view.Projects[0].Project.ID) != f.project || view.Projects[0].ProjectionPending || len(view.Cards) != 1 || view.Cards[0].Issue.Title != "Review the invitation flow" {
				t.Fatalf("ready work board did not return the visible issue: %s", response.Body.String())
			}
			for _, refusal := range refusals {
				requireNativeStatus(t, f.page(t, refusal.account, refusal.path), refusal.status)
			}
		})
		for _, path := range []string{
			"/projects", "/project-rank", "/members", "/fleet", "/plan", "/billing", "/secrets/" + flySpritesToken,
			"/monthly-budget", "/model-selection", "/integrations/slack",
			"/sprite-pool", "/work-view-preference", "/work-views",
			"/issue-notification-preference", "/daily-summary-preference",
			"/projects/" + f.project + "/work-view-preference",
			"/activity?project=" + f.project,
			"/reports?project=" + f.project + "&range=24h",
			"/usage?project=" + f.project,
			"/diagnostics?project=" + f.project,
			"/diagnostics/requests",
		} {
			t.Run(path, func(t *testing.T) {
				response := f.page(t, "owner", browserHostedOrganizationBase+path)
				requireNativeStatus(t, response, http.StatusOK)
			})
		}
		for _, path := range []string{"/entitlements", "/entitlements/runtime"} {
			t.Run(path, func(t *testing.T) {
				response := performHubAPIRequest(t, f.service, http.MethodGet, browserHostedOrganizationBase+path, strings.Repeat("e", 32), nil)
				requireNativeStatus(t, response, http.StatusOK)
			})
		}
		f.service.tokenUseWork.Wait()
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if len(guard.violations) > 0 {
			t.Fatalf("writer reads: %s", strings.Join(guard.violations, "\n"))
		}
		if guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
			t.Fatal("GET matrix did not use traced PostgreSQL read-only transactions")
		}
	})
}

type readPoolDeadlineKey struct{}

type readPoolDeadlineTrace struct {
	dispatchWaitPostgresTrace
}

func (p *readPoolDeadlineTrace) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context {
	ctx = p.dispatchWaitPostgresTrace.TraceAcquireStart(ctx, pool, data)
	if arm, ok := ctx.Value(readPoolDeadlineKey{}).(func(context.Context) context.Context); ok {
		return arm(ctx)
	}
	return ctx
}

func TestReadPoolExhaustionReturnsLoggedDeadline(t *testing.T) {
	t.Parallel()
	probe := &readPoolDeadlineTrace{}
	repository, _ := projectRecordPostgresWithAdmin(t, 3, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, probe)
	})
	var logs hostedLogBuffer
	service := openLocalPostgresHubTestService(t, repository, Config{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)), SecretKeys: secretTestKeys(t, "1", "1"),
	})
	organization := nativeOrganization{ID: "org_first", Name: "Example organization", Slug: "example-first"}
	seedPostgresHubAdmin(t, repository, service, organization)
	service.localOrganization = string(organization.ID)
	service.database.hostedOrganization = organization.ID
	service.requestMetrics.Organization = organization.ID
	f := newNativeFixture(t, service, organization.ID, "exhausted-readers")
	pool := probe.pool.Load()
	if pool == nil || pool.Stat().MaxConns() != 3 {
		t.Fatal("fixture did not expose its three-connection PostgreSQL pool")
	}

	var held []*pgxpool.Conn
	release := func() {
		for _, conn := range held {
			conn.Release()
		}
		held = nil
	}
	t.Cleanup(release)
	var acquireCtx context.Context
	var canceledBefore int64
	saturate := true
	handler := echo.New()
	handler.GET(hubapi.NativeBase+"/onboarding", func(c echo.Context) error {
		if !saturate {
			return service.getOnboardingPage(c)
		}
		ctx := c.Request().Context()
		for range pool.Stat().MaxConns() {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, conn)
		}
		stats := pool.Stat()
		if stats.AcquiredConns() != stats.MaxConns() {
			t.Fatalf("physical pool is not full: acquired=%d max=%d", stats.AcquiredConns(), stats.MaxConns())
		}
		canceledBefore = stats.CanceledAcquireCount()
		ctx = context.WithValue(ctx, readPoolDeadlineKey{}, func(ctx context.Context) context.Context {
			if acquireCtx == nil {
				var cancel context.CancelFunc
				acquireCtx, cancel = context.WithDeadline(ctx, time.Time{})
				t.Cleanup(cancel)
			}
			return acquireCtx
		})
		c.SetRequest(c.Request().WithContext(ctx))
		return service.getOnboardingPage(c)
	}, service.requireAPIScope(hubapi.APIScopeOperator, hubapi.APIScopeWorker, hubapi.APIScopeAdmin))

	request := newHubAPIRequest(t, http.MethodGet, f.base+"/onboarding", f.token, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(testenv.WaitContext(t, t.Context())))
	if acquireCtx == nil {
		t.Fatalf("request never reached physical acquire: status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Code != http.StatusInternalServerError || !errors.Is(acquireCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("pool exhaustion response=%d acquire context=%v", response.Code, acquireCtx.Err())
	}
	canceledAfter := pool.Stat().CanceledAcquireCount()
	if delta := canceledAfter - canceledBefore; delta != 1 {
		t.Fatalf("canceled PostgreSQL acquires = %d, want 1", delta)
	}
	var deadlines int
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "read_timeout_cause=read_deadline") {
			continue
		}
		deadlines++
		for _, field := range []string{"context deadline exceeded", "pool_full=true", "pool_in_use=3", "pool_max=3", "pool_canceled_acquires=" + strconv.FormatInt(canceledAfter, 10)} {
			if !strings.Contains(line, field) {
				t.Errorf("pool exhaustion log omitted %q: %s", field, line)
			}
		}
	}
	if deadlines != 1 {
		t.Fatalf("logged deadline records = %d, want 1: %s", deadlines, logs.String())
	}
	release()
	saturate = false
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newHubAPIRequest(t, http.MethodGet, f.base+"/onboarding", f.token, nil).WithContext(t.Context()))
	requireNativeStatus(t, response, http.StatusOK)
}

type auditDeadlineTrace struct{ inserts, bounded *atomic.Int64 }

func (trace auditDeadlineTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO organization_secret_audit") {
		trace.inserts.Add(1)
		if _, ok := ctx.Deadline(); ok {
			trace.bounded.Add(1)
		}
	}
	return ctx
}

func (auditDeadlineTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSecretUseAuditIsNotBoundedByReadDeadline(t *testing.T) {
	t.Parallel()
	var inserts, bounded atomic.Int64
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, auditDeadlineTrace{inserts: &inserts, bounded: &bounded})
	})
	service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org_first", Name: "org_first", Slug: "org-first"})
	f := newNativeFixture(t, service, "org_first", "audit-deadline")
	now := time.Now().UTC()
	configureTestSlack(t, f, &now, true)
	inserts.Store(0)
	bounded.Store(0)
	value, err := f.service.readSlackWebhook(t.Context(), f.project.OrganizationID, "test")
	if err != nil || len(value) == 0 {
		t.Fatalf("webhook read: %v", err)
	}
	clear(value)
	if inserts.Load() != 1 || bounded.Load() != 0 {
		t.Fatalf("secret use audit inserts=%d bounded by read deadline=%d", inserts.Load(), bounded.Load())
	}
}
