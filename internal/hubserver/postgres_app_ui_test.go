package hubserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAppBootstrapUsesTypedTenantAccess(t *testing.T) {
	originalClientFS := conversationClientFS
	conversationClientFS = fstest.MapFS{conversationClientShell: &fstest.MapFile{Data: []byte("<!doctype html><html><head></head><body></body></html>")}}
	t.Cleanup(func() { conversationClientFS = originalClientFS })
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Workspace = &WorkspaceConfig{Enabled: true}
	key := hostedSharedKey(31)
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
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
	if service.database.db != nil {
		t.Fatal("PostgreSQL browser bootstrap retained the SQLite database")
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
	if payload.Feature.Workspaces {
		t.Fatal("PostgreSQL app bootstrap advertised unavailable workspace storage")
	}
	if len(payload.Projects) != 1 || payload.Projects[0].ID != "project_granted" || payload.Projects[0].CanWrite || payload.Projects[0].CanManageRunners {
		t.Fatalf("PostgreSQL app bootstrap project access = %+v", payload.Projects)
	}
	session, err := service.accessWebSession(t.Context(), claims.Binding, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	credential, status, err := service.hostedSharedCredential(t.Context(), session, claims.Binding, "viewer")
	if err != nil || status != http.StatusOK {
		t.Fatalf("PostgreSQL project shell credential status = %d, error %v", status, err)
	}
	if err := service.authorizeProjectRecord(t.Context(), nativeScope{organization: "org_startup", project: "project_granted", credential: credential}, false); err != nil {
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
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Logger = discardLogger()
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_second"} {
		seedStartupOrganization(t, store.postgres, organization)
	}
	occupied, release, transactionDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		transactionDone <- store.postgres.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(tenantstore.Reader) error {
			close(occupied)
			<-release
			return nil
		})
	}()
	var releaseOnce sync.Once
	releaseServing := func() {
		releaseOnce.Do(func() {
			close(release)
			if err := <-transactionDone; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(releaseServing)
	<-occupied
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
				{name: "ready database with saturated serving queue", signed: true, ready: true, want: http.StatusOK},
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
					select {
					case <-entered:
						cancel()
						<-done
						t.Fatal("health entered saturated application admission")
					case <-done:
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
}
