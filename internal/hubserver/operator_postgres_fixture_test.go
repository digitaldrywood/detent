package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type operatorSecurityFixture struct {
	hostedSecurityFixture
	admin *sql.DB
}

func newOperatorSecurityFixture(t *testing.T, configure ...func(*Config)) operatorSecurityFixture {
	t.Helper()
	provider := newHostedSecurityProvider()
	plans := pilotHostedPlans()
	cfg := Config{GitHubDisabled: true, Hosted: &HostedConfig{Plans: &plans, OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", BootstrapSubject: "user_owner", PublicURL: "http://127.0.0.1:7777", StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, Provider: provider}}
	for _, apply := range configure {
		apply(&cfg)
	}
	admin := postgresStartupTemplate.Database(t)
	defaults := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.DatabaseBackend, cfg.Postgres = DatabasePostgres, defaults.Postgres
	cfg.RunDetails = &memoryRunDetails{}
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	store, err := openDatabase(t.Context(), cfg.normalized())
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_security", "org_security_second"} {
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			now := cfg.normalized().now().UTC().Truncate(time.Microsecond)
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			providerID := "org_provider"
			if org != "org_security" {
				providerID += "_second"
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: providerID, AllocationGeneration: 1}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "test_hub_admin", Name: "Example admin", Hash: apikey.HashToken(testHubAdminToken), Scope: "admin", Kind: "service", KeyScope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			key := bytes.Repeat([]byte{0x5a}, 32)
			envelope, err := cfg.SecretKeys.Seal(key, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: newNativeID("hub"), CursorKey: key}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(errors.Join(err, store.Close()))
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service := openTestService(t, cfg)
	project := tracker.ProjectID("prj_security")
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: project, Name: "private-project-sentinel", Profile: "native"}, States: states})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seedNativeFixturePolicy(t, service, "org_security/"+string(project))
	return operatorSecurityFixture{hostedSecurityFixture: hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}, admin: admin}
}

func (f operatorSecurityFixture) user(t *testing.T, name, role, email, grant, support string) hostedSecurityUser {
	t.Helper()
	user := f.hostedSecurityFixture.user(t, name, role, email, "", support)
	if grant != "" {
		f.grant(t, user, grant == "write", false)
	} else if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.ClearAccessMemberGrants(t.Context(), user.identity.Subject)
	}); err != nil {
		t.Fatal(err)
	}
	return user
}

func (f operatorSecurityFixture) grant(t *testing.T, user hostedSecurityUser, write, runner bool) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: user.identity.Subject, ProjectID: f.project, CanWrite: write, ManageRunner: runner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
}

type operatorSharedFixture struct {
	operatorSecurityFixture
	shared hostedSharedFixture
}

func newOperatorSharedFixture(t *testing.T, configure ...func(*Config)) operatorSharedFixture {
	t.Helper()
	key := hostedSharedKey(7)
	configure = append([]func(*Config){func(cfg *Config) {
		shared := hostedSharedTestConfig("", cfg.Hosted.Provider, key, 1)
		cfg.Hosted = shared.Hosted
	}}, configure...)
	f := newOperatorSecurityFixture(t, configure...)
	return operatorSharedFixture{operatorSecurityFixture: f, shared: hostedSharedFixture{hostedSecurityFixture: f.hostedSecurityFixture, key: key}}
}

func (f operatorSharedFixture) serve(t *testing.T, request hostedSharedRequest) *httptest.ResponseRecorder {
	t.Helper()
	return f.shared.serve(t, request)
}

type operatorKeyFixture struct {
	operatorSecurityFixture
	user    hostedSecurityUser
	browser func(string, string, url.Values) *httptest.ResponseRecorder
}

func newOperatorKeyFixture(t *testing.T, deployment, role string) (operatorKeyFixture, context.Context) {
	t.Helper()
	var f operatorSecurityFixture
	var shared operatorSharedFixture
	if deployment == "shared" {
		shared = newOperatorSharedFixture(t)
		f = shared.operatorSecurityFixture
	} else {
		f = newOperatorSecurityFixture(t)
	}
	user := f.user(t, "operator", role, "operator@example.test", "write", "")
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/key-mcp-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "key-connection", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	browser := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		if deployment == "shared" {
			return shared.serve(t, hostedSharedRequest{user: &user, method: method, target: "/organizations/org_security" + path, body: form.Encode(), form: true, csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
		}
		return f.request(t, user, method, path, form)
	}
	requireNativeStatus(t, browser(http.MethodPost, "/key-mcp-test", nil), http.StatusOK)
	ctx := <-contexts
	if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	return operatorKeyFixture{operatorSecurityFixture: f, user: user, browser: browser}, ctx
}
