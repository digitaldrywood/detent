package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryAuthRoutesRespectTenantAuthority(t *testing.T) {
	t.Parallel()
	f := newSharedRouteFixture(t)
	f.requestContext = func(ctx context.Context) context.Context {
		return ctx
	}
	for org, service := range f.services {
		config, err := service.config.Postgres.Connection.PoolConfig()
		if err != nil {
			t.Fatal(err)
		}
		pgtransport.Configure(config)
		config.MaxConns = int32(service.config.Postgres.Pool.MaxConnections)
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		measured, err := postgres.NewRepository(t.Context(), pool, service.config.Postgres.Pool)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := measured.Close(); err != nil {
				t.Error(err)
			}
		})
		originalAccess, originalTenant := service.config.AccessRepository, service.config.TenantRepository
		service.config.AccessRepository, service.config.TenantRepository = measured, measured
		provider := &browserHostedProvider{members: map[string]auth.Membership{}, invitations: map[string]auth.Invitation{}, inviteRoles: map[string]string{}}
		service.config.Hosted.Provider = provider
		if _, err := provider.CreateMembership(t.Context(), org+"_owner", "provider_"+org, "owner"); err != nil {
			t.Fatal(err)
		}
		seeded := 0
		for _, size := range []int{2} {
			for seeded < size {
				id := fmt.Sprintf("unrelated_%s_%d", org, seeded)
				if err := measured.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(org), ID: id, Name: id, Hash: id, Scope: "operator", Kind: "personal", ProjectAccess: "selected", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
						return err
					}
					return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: tracker.OrganizationID(org), UserID: id, Email: id + "@example.test", MembershipID: id, Role: "member", Active: true, PrincipalID: id, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				}); err != nil {
					t.Fatal(err)
				}
				seeded++
			}
			for _, route := range []struct {
				path, body string
				want       int
			}{
				{"/internal/v1/keys/admin", `{}`, http.StatusOK},
				{"/internal/v1/keys/context", `{}`, http.StatusOK},
				{"/internal/v1/owner/bootstrap", `{"organization_name":"Example"}`, http.StatusConflict},
				{"/internal/v1/invitations/accept", ``, http.StatusNoContent},
			} {
				body := route.body
				subject, email := org+"_owner", org+"_owner@example.test"
				if body == "" {
					id := fmt.Sprintf("budget_%s_%d", org, size)
					subject, email = id, id+"@example.test"
					invitation := auth.Invitation{ID: id, Email: email, OrganizationID: "provider_" + org, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
					provider.invitations[id], provider.inviteRoles[id] = invitation, "member"
					if err := measured.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
						return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: tracker.OrganizationID(org), ID: id, Email: invitation.Email, Role: "member", CreatedAt: time.Now(), ExpiresAt: invitation.ExpiresAt})
					}); err != nil {
						t.Fatal(err)
					}
					body = `{"invitation_id":"` + id + `"}`
				}
				response := f.request(t, org, route.path, body, func(c *cloudassert.Claims) { c.Subject, c.Email = subject, email })
				requireEntryAuthResponse(t, response.Code, response.Body.String(), route.want)
			}
		}
		service.config.AccessRepository, service.config.TenantRepository = originalAccess, originalTenant
		if err := measured.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
