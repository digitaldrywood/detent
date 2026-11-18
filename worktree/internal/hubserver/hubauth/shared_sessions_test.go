package hubauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSharedAccessSessionReadsExistingBindingWithoutWriting(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	organization := tracker.OrganizationID("org_shared_session")
	identity := auth.HostedIdentity{Subject: "user_example", OrganizationID: "provider_example", SessionID: "session_example", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	row := accessstore.Session{OrganizationID: organization, Hash: "binding_example", Email: "member@example.test", Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, CreatedAt: now, ExpiresAt: identity.ExpiresAt}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: identity.OrganizationID}); err != nil {
			return err
		}
		return w.CreateAccessSession(t.Context(), row)
	}); err != nil {
		t.Fatal(err)
	}
	counted := &countingTenantRepository{Repository: repository}
	service := hubauth.New(hubauth.Deps{AccessRepository: counted, Organization: organization, Hosted: &hubauth.Hosted{OrganizationID: string(organization)}, Now: func() time.Time { return now }})
	claims := cloudassert.Claims{Kind: cloudassert.KindBrowser, Audience: string(organization), Subject: identity.Subject, ProviderOrganization: identity.OrganizationID, ProviderSession: identity.SessionID, SessionCreatedAt: identity.CreatedAt, SessionExpiresAt: identity.ExpiresAt, AccessExpiresAt: identity.ExpiresAt, Email: row.Email, Role: "admin", Binding: row.Hash}
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	if _, _, err := service.SharedAccessSession(t.Context(), c, claims); err != nil {
		t.Fatalf("existing binding authentication: %v", err)
	}
	if counted.reads != 1 || counted.writes != 0 {
		t.Fatalf("existing binding repository calls = reads %d, writes %d; want reads 1, writes 0", counted.reads, counted.writes)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessSession(t.Context(), row.Hash, now)
	}); err != nil {
		t.Fatal(err)
	}
	counted.reads, counted.writes = 0, 0
	if _, _, err := service.SharedAccessSession(t.Context(), c, claims); !errors.Is(err, auth.ErrInvalidSession) || errors.Is(err, tenantauth.ErrUnavailable) {
		t.Fatalf("revoked binding error=%v, want invalid session without unavailable", err)
	}
	if counted.reads != 1 || counted.writes != 0 {
		t.Fatalf("revoked binding repository calls = reads %d, writes %d; want reads 1, writes 0", counted.reads, counted.writes)
	}
}

type countingTenantRepository struct {
	tenantstore.Repository
	reads   int
	writes  int
	readErr error
}

func (r *countingTenantRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads++
	if r.readErr != nil {
		return r.readErr
	}
	return r.Repository.ReadTenant(ctx, organization, workload, read)
}

func (r *countingTenantRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.writes++
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}
