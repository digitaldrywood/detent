package hostedplans

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/singleflight"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const testHostedPlanAdminToken = "plan-admin-token-for-isolated-tests-2195"
const testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"

type hubDatabase struct {
	billingRepository  tenantstore.Repository
	billingAuditStore  attachment.DetailStore
	hostedOrganization tracker.OrganizationID
	hostedRuntime      atomic.Pointer[RuntimeConfig]
	runtimeLoads       singleflight.Group
	runtimeBilling     *HostedBillingConfig
	hostedBilling      bool
	logger             *slog.Logger
	now                func() time.Time
}

func (d *hubDatabase) plansService() *Service {
	return New(Deps{Repository: d.billingRepository, Details: d.billingAuditStore, Organization: d.hostedOrganization, Runtime: &d.hostedRuntime, Loads: &d.runtimeLoads, RuntimeBilling: d.runtimeBilling, HostedBilling: d.hostedBilling, Logger: d.logger, Now: d.now, Configured: func(string, float64, bool) { d.hostedBilling = true }})
}

type hubConfig struct {
	Hosted *HostedConfig
	Logger *slog.Logger
	now    func() time.Time
}

type hubService struct {
	database  *hubDatabase
	config    hubConfig
	echo      *echo.Echo
	runtimeMu sync.Mutex
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func (s *hubService) httpDeps() HTTPDeps {
	return HTTPDeps{
		Plans:     func() *Service { return s.database.plansService() },
		Hosted:    func() *HostedConfig { return s.config.Hosted },
		Logger:    func() *slog.Logger { return s.config.Logger },
		Now:       func() time.Time { return s.config.now() },
		RuntimeMu: &s.runtimeMu,
		Error:     renderTestError,
		InvalidRequest: func(c echo.Context, err error) error {
			return c.JSON(http.StatusUnprocessableEntity, hubapi.APIErrorResponse{Code: "invalid_request", Message: err.Error()})
		},
		JSONError: func(c echo.Context, status int, message string) error {
			return c.JSON(status, hubapi.APIErrorResponse{Code: http.StatusText(status), Message: message})
		},
		Administrator: func(echo.Context) (string, error) { return "", errors.New("hosted administrator unavailable") },
		Render:        func(c echo.Context, status int, _ templates.HostedPageData) error { return c.NoContent(status) },
		PageError:     func(c echo.Context, status int, _ string) error { return c.NoContent(status) },
	}
}

func renderTestError(c echo.Context, err error) error {
	var failure *hubapi.NativeError
	if errors.As(hubapi.FromFeatureError(err), &failure) {
		return c.JSON(failure.Status, hubapi.APIErrorResponse{Code: failure.Code, Message: failure.Message})
	}
	return c.JSON(http.StatusInternalServerError, hubapi.APIErrorResponse{Code: "internal_error", Message: "Internal error"})
}

func (s *hubService) http() *HTTPService { return NewHTTP(s.httpDeps()) }

func (s *hubService) readHostedRuntime(c echo.Context) error { return s.http().ReadRuntime(c) }

func (s *hubService) reloadHostedRuntime(c echo.Context) error { return s.http().ReloadRuntime(c) }

func (s *hubService) refreshHostedRuntime(next echo.HandlerFunc) echo.HandlerFunc {
	return s.http().RefreshRuntime(next)
}

func (s *hubService) applyHostedRuntime(ctx context.Context, update *HostedRuntimeUpdate) error {
	return s.http().applyRuntime(ctx, update)
}

func (s *hubService) hostedBillingConfig() *HostedBillingConfig { return s.http().billingConfig() }

func (s *hubService) Handler() http.Handler { return s.echo }

func (s *hubService) accessOrganization() tracker.OrganizationID {
	return s.database.hostedOrganization
}

func performHubAPIRequest(t *testing.T, service *hubService, method string, path string, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
}

func runtimeProbeService(repo tenantstore.Repository) *hubService {
	d := &hubDatabase{billingRepository: repo, logger: discardLogger(), now: time.Now}
	d.hostedRuntime.Store(&RuntimeConfig{Revision: "loaded", Plans: PilotPlans()})
	return &hubService{database: d, config: hubConfig{Logger: discardLogger(), now: time.Now, Hosted: &HostedConfig{OrganizationID: "org_example", EntitlementAdminToken: []byte(testHostedPlanAdminToken)}}}
}

type runtimeBillingProvider struct{}

func (runtimeBillingProvider) Reconcile(context.Context, billing.Binding) (billing.Snapshot, error) {
	return billing.Snapshot{}, nil
}

func (runtimeBillingProvider) Checkout(context.Context, billing.CheckoutRequest) (billing.Session, error) {
	return billing.Session{}, nil
}

func (runtimeBillingProvider) Portal(context.Context, billing.Binding, string, string) (billing.Session, error) {
	return billing.Session{}, nil
}

func newRuntimePostgresService(t *testing.T, plans HostedPlansConfig, enableBilling bool) (*hubService, *sql.DB) {
	t.Helper()
	repo, admin := postgresWithAdmin(t, 4)
	if err := repo.WriteTenant(t.Context(), "org_browser_preview", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_browser_preview", Name: "Example", CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	cfg := hubConfig{Logger: discardLogger(), now: time.Now, Hosted: &HostedConfig{OrganizationID: "org_browser_preview", Plans: &plans, EntitlementAdministrator: "test-operator", EntitlementAdminToken: []byte(testHostedPlanAdminToken)}}
	if enableBilling {
		cfg.Hosted.Billing = &HostedBillingConfig{AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_2196_secret"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: runtimeBillingProvider{}, Prices: []HostedBillingPrice{{PriceID: "price_fixture", Label: "Extended pilot", Plan: plans.Plans[1].PlanReference}}}
	}
	d := &hubDatabase{billingRepository: repo, hostedOrganization: "org_browser_preview", billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger(), runtimeBilling: cfg.Hosted.Billing}
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), PlansInput(cfg.Hosted))); err != nil {
		t.Fatal(err)
	}
	if enableBilling {
		if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), PlansInput(cfg.Hosted))); err != nil {
			t.Fatal(err)
		}
	}
	s := &hubService{database: d, config: cfg, echo: echo.New()}
	s.echo.GET("/api/v2/organizations/:organization/entitlements/runtime", s.readHostedRuntime, s.refreshHostedRuntime)
	s.echo.POST("/api/v2/organizations/:organization/entitlements/runtime", s.reloadHostedRuntime, s.refreshHostedRuntime)
	return s, admin
}

func runtimeAuditCount(t *testing.T, s *hubService, action, reason string) int {
	t.Helper()
	count := 0
	if err := hubapi.FromFeatureError(s.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
		rows, err := r.BillingPlanAudits(t.Context(), 0, 100)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Action != action || row.ActorID != "test-operator" {
				continue
			}
			raw, err := s.database.billingAuditStore.ReadDetail(t.Context(), string(s.database.hostedOrganization), row.Reference.ArchiveReference())
			if err != nil {
				return err
			}
			var command PlanCommand
			if err := json.Unmarshal(raw, &command); err != nil {
				return err
			}
			if reason == "" || command.Reason == reason {
				count++
			}
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	return count
}

func configuredPlanPostgresServices(t *testing.T, plans HostedPlansConfig, repo *postgres.Repository) []*hubService {
	t.Helper()
	var services []*hubService
	suffix := sha256.Sum256([]byte(t.Name()))
	for _, name := range []string{"first", "second"} {
		org := tracker.OrganizationID(fmt.Sprintf("org_%x_%s", suffix[:6], name))
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &hubDatabase{billingRepository: repo, hostedOrganization: org, billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger()}
		s := &hubService{database: d, config: hubConfig{Hosted: &HostedConfig{OrganizationID: string(org), Plans: &plans, EntitlementAdministrator: "operator"}, now: time.Now, Logger: discardLogger()}}
		if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), PlansInput(s.config.Hosted))); err != nil {
			t.Fatal(err)
		}
		services = append(services, s)
	}
	return services
}

type entitlementFixture struct {
	service *hubService
	admin   *sql.DB
	http    *hubtest.HTTPHarness
}

func newEntitlementSessionFixture(t *testing.T) entitlementFixture {
	t.Helper()
	repo, admin := postgresWithAdmin(t, 4)
	const organization = tracker.OrganizationID("org_security")
	hubtest.Organization(t, repo, hubtest.OrganizationOptions{ID: organization})
	hubtest.Project(t, repo, organization, hubtest.ProjectOptions{ID: "prj_security", Name: "private-project-sentinel"})
	legacyPlans := PilotPlans()
	s := &hubService{database: &hubDatabase{billingRepository: repo, hostedOrganization: organization, billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger()}, config: hubConfig{Logger: discardLogger(), now: time.Now, Hosted: &HostedConfig{OrganizationID: string(organization), Plans: &legacyPlans}}}
	if err := hubapi.FromFeatureError(s.database.plansService().ConfigurePlans(t.Context(), PlansInput(s.config.Hosted))); err != nil {
		t.Fatal(err)
	}
	h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repo, Organization: organization, Hosted: &hubauth.Hosted{}}, func(g *echo.Group, harness hubtest.HTTPDeps) {
		deps := s.httpDeps()
		deps.Administrator = func(c echo.Context) (string, error) {
			credential, _, err := harness.Auth.HostedCredential(c.Request().Context(), c)
			if err != nil {
				return "", err
			}
			if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return "", auth.ErrHostedIdentity
			}
			return credential.HostedRole, nil
		}
		deps.Render = func(c echo.Context, status int, data templates.HostedPageData) error {
			data.OrganizationID = s.config.Hosted.OrganizationID
			c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
			c.Response().WriteHeader(status)
			return templates.HostedPage(data).Render(c.Request().Context(), c.Response())
		}
		deps.PageError = func(c echo.Context, status int, message string) error {
			return deps.Render(c, status, templates.HostedPageData{Mode: "denied", Title: "Access unavailable", Error: message})
		}
		RegisterRoutes(g, deps)
	})
	s.echo = h.Echo
	return entitlementFixture{service: s, admin: admin, http: h}
}

func (f entitlementFixture) user(t *testing.T, _, role, email, _, _ string) hubtest.HTTPActor {
	t.Helper()
	return f.http.MemberSession(t, hubtest.MemberOptions{Role: role, Email: email})
}

func (f entitlementFixture) request(t *testing.T, user hubtest.HTTPActor, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.http.Request(t, user, method, target, body).Recorder
}

func newEntitlementFixture(t *testing.T) entitlementFixture {
	t.Helper()
	repo, admin := postgresWithAdmin(t, 4)
	const organization = tracker.OrganizationID("org_security")
	if err := repo.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	legacyPlans := PilotPlans()
	cfg := hubConfig{Logger: discardLogger(), now: time.Now, Hosted: &HostedConfig{OrganizationID: string(organization), Plans: &legacyPlans}}
	d := &hubDatabase{billingRepository: repo, hostedOrganization: organization, billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger()}
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), PlansInput(cfg.Hosted))); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().ConfigureBilling(t.Context(), PlansInput(cfg.Hosted))); err != nil {
		t.Fatal(err)
	}
	s := &hubService{database: d, config: cfg, echo: echo.New()}
	RegisterRoutes(s.echo.Group(""), s.httpDeps())
	return entitlementFixture{service: s, admin: admin}
}

func hostedTestPlans(t *testing.T, f entitlementFixture, limits map[string]int64) HostedPlansConfig {
	t.Helper()
	service := f.service
	service.config.Hosted.EntitlementAdminToken = []byte(testHostedPlanAdminToken)
	service.config.Hosted.EntitlementAdministrator = "test-operator"
	config := PilotPlans()
	free := config.Plans[0]
	free.ID = "test_free"
	maps.Copy(free.Allowances, limits)
	paid := free
	paid.ID = "test_paid"
	paid.Allowances = maps.Clone(free.Allowances)
	paid.Allowances["projects"] = 20
	paid.Allowances["concurrent_work"] = 20
	paid.Features = append(slices.Clone(free.Features), "hosted_artifacts")
	transition := PilotPlans()
	transition.Plans = append(transition.Plans, free, paid)
	cfg := *service.config.Hosted
	cfg.Plans = &transition
	if err := hubapi.FromFeatureError(service.database.plansService().ConfigurePlans(t.Context(), PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_id=$1,base_version=$2 WHERE organization_id=$3", free.ID, free.Version, service.database.hostedOrganization); err != nil {
		t.Fatal(err)
	}
	config.Plans = []HostedPlan{free, paid}
	config.Base = free.PlanReference
	cfg.Plans = &config
	if err := hubapi.FromFeatureError(service.database.plansService().ConfigurePlans(t.Context(), PlansInput(&cfg))); err != nil {
		t.Fatal(err)
	}
	return config
}

type leaseTestClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *leaseTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}
