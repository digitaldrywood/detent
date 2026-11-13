package hubserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAppBootstrapUsesTypedTenantAccess(t *testing.T) {
	originalClientFS := conversationClientFS
	conversationClientFS = fstest.MapFS{conversationClientShell: &fstest.MapFile{Data: []byte("<!doctype html><html><head></head><body></body></html>")}}
	t.Cleanup(func() { conversationClientFS = originalClientFS })
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	cfg.Hosted.WorkOSOrganizationID = "provider_" + cfg.Hosted.OrganizationID
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Workspace = &WorkspaceConfig{Enabled: true}
	cfg.Conversation = &ConversationConfig{Enabled: true, Model: "configured-model", ReasoningEffort: "medium"}
	key := hostedSharedKey(31)
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup", cfg.Hosted)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.postgres.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: "credential_viewer", Name: "Example viewer", Hash: apikey.HashToken("example_viewer_token"), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_viewer", MembershipID: "membership_viewer", ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_startup", UserID: "user_viewer", Email: "viewer@example.test", MembershipID: "membership_viewer", Role: "viewer", Active: true, PrincipalID: "credential_viewer", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for _, project := range []projectstore.Record{
			{Core: corestore.Project{OrganizationID: "org_startup", ID: "project_granted", Name: "Granted project", Slug: "granted-project", Profile: "native"}, States: nativeFixtureStates()},
			{Core: corestore.Project{OrganizationID: "org_startup", ID: "project_hidden", Name: "Hidden project", Slug: "hidden-project", Profile: "native"}, States: nativeFixtureStates()},
		} {
			if _, err := w.CreateProjectRecord(t.Context(), project); err != nil {
				return err
			}
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "user_viewer", ProjectID: "project_granted"}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_viewer", ProjectID: "project_granted"})
	}); err != nil {
		t.Fatal(err)
	}
	cfg.Clock = func() time.Time { return now }
	seedStartupOrganization(t, store.postgres, "org_catalog_other")
	if err := store.postgres.WriteTenant(t.Context(), "org_catalog_other", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_catalog_other", ID: "project_granted", Name: "Other tenant", Slug: "other-tenant", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seedRunner := func(organization tracker.OrganizationID, project tracker.ProjectID, id string, report providercapacity.Report) {
		t.Helper()
		if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: id, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: apikey.HashToken(string(organization) + "/" + id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: new(now.Add(time.Hour)), ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: project}); err != nil {
				return err
			}
			enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken("enrollment/" + string(organization) + "/" + id), Scope: "projects", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "test_hub_admin"}
			if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, id, tracker.MachineID(id), now); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: enrollment.ID, Scope: "projects", Operations: enrollment.Operations, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			return w.ReplaceRunnerProviderReports(t.Context(), id, []providercapacity.Report{report})
		}); err != nil {
			t.Fatal(err)
		}
	}
	report := providercapacity.Report{Provider: "example", Backend: "example", AccountAlias: "example", Billing: "subscription", MaxConcurrent: 1, Availability: "exhausted", ObservedAt: now, Models: []string{"z-model", "a-model"}, ModelDetails: []providercapacity.ModelDetail{{ID: "z-model", Label: "First model", Provider: "model-provider", ReasoningEfforts: []string{"medium", "max"}, DefaultReasoningEffort: "max", Default: true, Legacy: true}}}
	seedRunner("org_startup", "project_granted", "runner_a", report)
	report.Models = []string{"z-model", "b-model"}
	report.ModelDetails = []providercapacity.ModelDetail{{ID: "z-model", Label: "Later duplicate", ReasoningEfforts: []string{"low"}}}
	seedRunner("org_startup", "project_granted", "runner_b", report)
	report.Models, report.ModelDetails = []string{"hidden-model"}, nil
	seedRunner("org_startup", "project_hidden", "runner_hidden", report)
	report.Models = []string{"other-tenant-model"}
	seedRunner("org_catalog_other", "project_granted", "runner_a", report)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open PostgreSQL Hub = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.postgres == nil {
		t.Fatal("browser bootstrap requires PostgreSQL")
	}

	target := "/organizations/org_startup/app/bootstrap"
	requestID, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	claims := cloudassert.Claims{
		Issuer: "entry", Audience: "org_startup", Generation: 1, Kind: cloudassert.KindBrowser,
		Method: http.MethodGet, Path: target, BodyDigest: cloudassert.BodyDigest(nil),
		IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: requestID,
		Subject: "user_viewer", Email: "viewer@example.test", ProviderOrganization: "provider_org_startup", ProviderSession: "session_viewer",
		SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), AccessExpiresAt: now.Add(time.Hour), Role: "viewer",
		Binding: cloudassert.AuthorizationBinding("shared-user_viewer", "org_startup", "session_viewer"),
		CSRF:    cloudassert.CSRFToken("shared-user_viewer", "org_startup"),
	}
	serveSigned := func(target, organizationSlug string) *httptest.ResponseRecorder {
		t.Helper()
		requestID, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		claims.Path, claims.OrganizationSlug, claims.ID = target, organizationSlug, requestID
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set(cloudassert.Header, assertion)
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		return response
	}
	response := serveSigned(target, "")
	if response.Code != http.StatusOK {
		t.Fatalf("PostgreSQL app bootstrap status = %d: %s", response.Code, response.Body.String())
	}
	var payload appBootstrap
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Organization.ID != "org_startup" || payload.Organization.Name != "Example" || payload.Organization.Slug != "example-startup" || payload.Actor.Role != "viewer" {
		t.Fatalf("PostgreSQL app bootstrap identity = %+v", payload)
	}
	if !payload.Feature.Workspaces {
		t.Fatal("PostgreSQL app bootstrap hid configured workspace storage")
	}
	if len(payload.Projects) != 1 || payload.Projects[0].ID != "project_granted" || payload.Projects[0].CanWrite || payload.Projects[0].CanManageRunners {
		t.Fatalf("PostgreSQL app bootstrap project access = %+v", payload.Projects)
	}
	modelIDs := make([]string, 0, len(payload.Preferences.Models))
	for _, choice := range payload.Preferences.Models {
		modelIDs = append(modelIDs, choice.ID)
	}
	if !slices.Equal(modelIDs, []string{"auto", "z-model", "a-model", "b-model", "configured-model"}) {
		t.Fatalf("authorized bootstrap model order = %v", modelIDs)
	}
	choice := payload.Preferences.Models[1]
	if choice.Label != "First model" || choice.Provider != "model-provider" || !choice.BackendDefault || !choice.Legacy || choice.DefaultEffort != "max" || !slices.Equal(choice.Efforts, []string{"medium", "max"}) || choice.Default || !payload.Preferences.Models[4].Default {
		t.Fatalf("bootstrap model details/defaults = %+v", payload.Preferences.Models)
	}
	other, err := service.appBootstrapPreferences(t.Context(), "org_catalog_other", []appBootstrapProject{{ID: "project_granted"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Models) != 3 || other.Models[1].ID != "other-tenant-model" || other.Models[2].ID != "configured-model" {
		t.Fatalf("second tenant catalog = %+v", other.Models)
	}
	session, err := service.accessWebSession(t.Context(), claims.Binding, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	credential, status, err := service.hostedSharedCredential(t.Context(), session, claims.Binding, "viewer")
	if err != nil || status != http.StatusOK {
		t.Fatalf("PostgreSQL project shell credential status = %d, error %v", status, err)
	}
	if err := service.authorizeProjectRecord(t.Context(), hubscope.NativeScope{Organization: "org_startup", Project: "project_granted", Credential: credential}, false); err != nil {
		t.Fatalf("PostgreSQL granted project authorization: %v", err)
	}
	if response := serveSigned("/example-startup/work", "example-startup"); response.Code != http.StatusOK {
		t.Fatalf("PostgreSQL app shell status = %d: %s", response.Code, response.Body.String())
	}
	if response := serveSigned("/example-startup/projects/project_granted", "example-startup"); response.Code != http.StatusOK {
		t.Fatalf("PostgreSQL granted project shell status = %d: %s", response.Code, response.Body.String())
	}
	if response := serveSigned("/example-startup/projects/project_hidden", "example-startup"); response.Code != http.StatusNotFound {
		t.Fatalf("PostgreSQL hidden project shell status = %d, want %d", response.Code, http.StatusNotFound)
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_startup/unavailable", nil)
	apiResponse := httptest.NewRecorder()
	service.Handler().ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned PostgreSQL API path status = %d, want %d", apiResponse.Code, http.StatusUnauthorized)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.DeleteAccessProjectGrant(t.Context(), "user_viewer", "project_granted"); err != nil {
			return err
		}
		return w.DeleteAccessTokenGrant(t.Context(), "credential_viewer", "project_granted")
	}); err != nil {
		t.Fatal(err)
	}
	response = serveSigned(target, "")
	requireNativeStatus(t, response, http.StatusOK)
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Projects) != 0 || len(payload.Preferences.Models) != 2 || payload.Preferences.Models[0].ID != "auto" || payload.Preferences.Models[1].ID != "configured-model" {
		t.Fatalf("bootstrap after grant revocation = projects:%+v models:%+v", payload.Projects, payload.Preferences.Models)
	}

}

type healthAdmissionRepository struct {
	tenantstore.Repository
	entered chan struct{}
}

func (r healthAdmissionRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	return r.Repository.ReadTenant(ctx, organization, workload, read)
}

func TestPostgresSharedHealthBypassesSaturatedServingAdmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		pool    postgres.PoolConfig
		serving int
	}{
		{"max3", postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1}, 1},
		{"max5", postgres.PoolConfig{MaxConnections: 5, ServingConnections: 4, BackgroundConnections: 1}, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPostgresSharedHealthWithSaturatedWorkloads(t, test.pool, test.serving)
		})
	}
}

func testPostgresSharedHealthWithSaturatedWorkloads(t *testing.T, config postgres.PoolConfig, expectedServing int) {
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Logger = discardLogger()
	cfg.Postgres.Pool = config
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_second"} {
		seedStartupOrganization(t, store.postgres, organization)
	}
	serving, background := store.postgres.WorkloadCapacities()
	release, transactionDone := make(chan struct{}), make(chan error, serving+background)
	held := 0
	var releaseOnce sync.Once
	releaseServing := func() {
		releaseOnce.Do(func() {
			close(release)
			for range held {
				if err := <-transactionDone; err != nil {
					t.Error(err)
				}
			}
		})
	}
	t.Cleanup(releaseServing)
	workloads := []corestore.Workload{}
	for range background {
		workloads = append(workloads, corestore.Background)
	}
	for range serving {
		workloads = append(workloads, corestore.Serving)
	}
	for index, workload := range workloads {
		occupied := make(chan struct{})
		organization := []tracker.OrganizationID{"org_startup", "org_second"}[index%2]
		held++
		go func() {
			transactionDone <- store.postgres.ReadTenant(t.Context(), organization, workload, func(tenantstore.Reader) error {
				close(occupied)
				<-release
				return nil
			})
		}()
		select {
		case <-occupied:
		case err := <-transactionDone:
			held--
			t.Fatalf("held transaction exited before readiness: %v", err)
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("held transaction did not start")
		}
	}
	if stats := store.postgres.Stats(); int(stats.AcquiredConns()) != serving+background || int(stats.MaxConns()) != config.MaxConnections {
		t.Fatalf("admitted workload transactions did not occupy their physical connections: %+v", stats)
	}
	t.Logf("held all admitted workloads: serving=%d background=%d physical_max=%d", serving, background, config.MaxConnections)
	key := hostedSharedKey(41)
	for _, organization := range []string{"org_startup", "org_second"} {
		t.Run(organization, func(t *testing.T) {
			configuration := cfg
			hosted := *cfg.Hosted
			configuration.Hosted = &hosted
			configuration.Hosted.OrganizationID = organization
			configuration.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
			if err := configuration.Hosted.SharedEntry.validate(organization); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{}, 1)
			configuration.TenantRepository = healthAdmissionRepository{Repository: store.postgres, entered: entered}
			configuration.AccessRepository = configuration.TenantRepository
			service := &Service{config: configuration, database: store}
			service.ready.Store(true)
			handler := echo.New()
			handler.Pre(service.hostedSharedEntry)
			handler.Use(service.hostedBoundary)
			handler.POST("/internal/v1/health", service.hostedSharedHealth)
			for _, test := range []struct {
				name   string
				signed bool
				ready  bool
				want   int
				closed bool
			}{
				{name: "ready database with saturated serving and background queues", signed: true, ready: true, want: http.StatusOK},
				{name: "startup remains unavailable", signed: true, want: http.StatusServiceUnavailable},
				{name: "unsigned remains unauthorized", ready: true, want: http.StatusUnauthorized},
				{name: "database failure remains unavailable", signed: true, ready: true, closed: true, want: http.StatusServiceUnavailable},
			} {
				if test.closed && organization != "org_second" {
					continue
				}
				t.Run(test.name, func(t *testing.T) {
					if test.closed {
						releaseServing()
						if err := store.Close(); err != nil {
							t.Fatal(err)
						}
					}
					service.ready.Store(test.ready)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					request := httptest.NewRequest(http.MethodPost, "/internal/v1/health", strings.NewReader("{}")).WithContext(ctx)
					if test.signed {
						id, err := cloudassert.NewID()
						if err != nil {
							t.Fatal(err)
						}
						now := time.Now()
						assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: organization, Generation: 1, Kind: cloudassert.KindService, OrganizationSlug: "example-" + strings.TrimPrefix(organization, "org_"), Method: http.MethodPost, Path: "/internal/v1/health", BodyDigest: cloudassert.BodyDigest([]byte("{}")), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id})
						if err != nil {
							t.Fatal(err)
						}
						request.Header.Set(cloudassert.Header, assertion)
					}
					response := httptest.NewRecorder()
					done := make(chan struct{})
					go func() { defer close(done); handler.ServeHTTP(response, request) }()
					testenv.WaitUntil(t, ctx, func() error {
						select {
						case <-done:
							return nil
						case <-entered:
							cancel()
							<-done
							t.Fatal("health entered saturated application admission")
						default:
						}
						return errors.New("health request has not completed")
					})
					select {
					case <-entered:
						t.Fatal("health entered saturated application admission")
					default:
					}
					if response.Code != test.want {
						t.Fatalf("health status = %d, want %d: %s", response.Code, test.want, response.Body.String())
					}
					if test.want == http.StatusOK {
						var health MigrationHealth
						if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
							t.Fatal(err)
						}
						if !health.SchemaFactsComplete || health.SchemaVersion != store.schemaVersion || health.SchemaFingerprint != store.schemaFingerprint {
							t.Fatalf("health lost schema facts: %+v", health)
						}
					}
				})
			}
		})
	}
	if serving != expectedServing || background != config.BackgroundConnections {
		t.Fatalf("effective workload capacities = %d/%d, want %d/%d", serving, background, expectedServing, config.BackgroundConnections)
	}
}
