package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type postgresAuthorityFaultTrace struct {
	table, id string
	hits      int
}

func (f *postgresAuthorityFaultTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if f.table != "" && strings.Contains(query.SQL, "FROM "+f.table+" WHERE") && len(query.Args) > 1 && query.Args[1] == f.id {
		f.hits++
		failed, cancel := context.WithCancel(ctx)
		cancel()
		return failed
	}
	return ctx
}
func (*postgresAuthorityFaultTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestPostgresAuthorityLookupOutages(t *testing.T) {
	fixture := newSharedRouteFixture(t)
	for org, service := range fixture.services {
		config, err := service.config.Postgres.Connection.PoolConfig()
		if err != nil {
			t.Fatal(err)
		}
		pgtransport.Configure(config)
		config.MaxConns = int32(service.config.Postgres.Pool.MaxConnections)
		trace := &postgresAuthorityFaultTrace{}
		config.ConnConfig.Tracer = trace
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := postgres.NewRepository(t.Context(), pool, service.config.Postgres.Pool)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repository.Close(); err != nil {
				t.Error(err)
			}
		})
		now := time.Now()
		owner, issued, sessionHash := org+"_owner", org+"_issued", org+"_session"
		project := tracker.ProjectID("project_" + org)
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: tracker.OrganizationID(org), ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(org), ID: issued, Name: "Example issued credential", Hash: issued, Scope: "operator", NativeOnly: true, KeyScope: "admin", UserID: owner, MembershipID: owner, ProjectAccess: "selected", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: tracker.OrganizationID(org), Hash: sessionHash, Email: owner + "@example.test", Subject: owner, ProviderOrganizationID: "provider_" + org, SessionID: sessionHash, IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: tracker.OrganizationID(org), UserID: owner, ProjectID: project, CanWrite: true}); err != nil {
				return err
			}
			for _, token := range []string{owner, issued} {
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(org), TokenID: token, ProjectID: project}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		base := accessstore.Authority{CredentialID: issued, Hash: issued, UserID: owner, MembershipID: owner, SessionHash: sessionHash, ProjectID: project, Write: true, Role: "owner"}
		request := func(t *testing.T, authority accessstore.Authority, want int) {
			t.Helper()
			endpoint := echo.New()
			endpoint.GET("/authorize", func(c echo.Context) error {
				ctx := c.Request().Context()
				err := repository.ReadTenant(ctx, tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error { _, err := r.AuthorizeAccess(ctx, authority, now); return err })
				if err != nil {
					return service.nativeAPIError(c, operatorAuthorizationError(err))
				}
				return c.NoContent(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			endpoint.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/authorize", nil))
			requireEntryAuthResponse(t, response.Code, response.Body.String(), want)
		}
		t.Run(org+"/allowed", func(t *testing.T) { request(t, base, http.StatusNoContent) })
		for _, fault := range []struct{ name, table, id string }{
			{"principal", "api_tokens", owner},
			{"session", "hosted_sessions", sessionHash},
			{"project grant", "hosted_project_grants", owner},
			{"principal token grant", "token_grants", owner},
			{"issued token grant", "token_grants", issued},
		} {
			t.Run(org+"/"+fault.name, func(t *testing.T) {
				trace.table, trace.id, trace.hits = fault.table, fault.id, 0
				defer func() { trace.table, trace.id = "", "" }()
				request(t, base, http.StatusServiceUnavailable)
				if trace.hits != 1 {
					t.Fatalf("fault queries=%d want=1", trace.hits)
				}
			})
		}
		for _, control := range []struct {
			name   string
			change func(*accessstore.Authority)
		}{
			{"missing session", func(a *accessstore.Authority) { a.SessionHash = "missing" }},
			{"missing project grant", func(a *accessstore.Authority) { a.ProjectID = "missing" }},
			{"wrong role", func(a *accessstore.Authority) { a.Role = "viewer" }},
		} {
			t.Run(org+"/"+control.name, func(t *testing.T) {
				authority := base
				control.change(&authority)
				request(t, authority, http.StatusForbidden)
			})
		}
		t.Run(org+"/revoked principal", func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), owner, now) }); err != nil {
				t.Fatal(err)
			}
			request(t, base, http.StatusForbidden)
		})
	}
}
