package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryAuthFaultRepository struct {
	tenantstore.Repository
	stage string
	err   error
}

func (r entryAuthFaultRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(entryAuthFaultReader{Reader: reader, stage: r.stage, err: r.err})
	})
}

func (r entryAuthFaultRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if r.stage == "write" {
		return r.err
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

type entryAuthFaultReader struct {
	tenantstore.Reader
	stage string
	err   error
}

func (r entryAuthFaultReader) AccessTenant(ctx context.Context) (accessstore.Tenant, error) {
	if r.stage == "tenant" {
		return accessstore.Tenant{}, r.err
	}
	return r.Reader.AccessTenant(ctx)
}

func (r entryAuthFaultReader) AccessMember(ctx context.Context, user string) (accessstore.Member, error) {
	if r.stage == "member" {
		return accessstore.Member{}, r.err
	}
	return r.Reader.AccessMember(ctx, user)
}

func (r entryAuthFaultReader) AccessCredential(ctx context.Context, id string) (accessstore.Credential, error) {
	if r.stage == "credential" {
		return accessstore.Credential{}, r.err
	}
	return r.Reader.AccessCredential(ctx, id)
}

func (r entryAuthFaultReader) AuthorizeAccess(ctx context.Context, authority accessstore.Authority, now time.Time) (accessstore.Member, error) {
	if r.stage == "authority" {
		return accessstore.Member{}, r.err
	}
	return r.Reader.AuthorizeAccess(ctx, authority, now)
}

type entryAuthProvider struct {
	*browserHostedProvider
	membershipErr error
}

func (p *entryAuthProvider) Memberships(ctx context.Context, user, org string) ([]auth.Membership, error) {
	if p.membershipErr != nil {
		return nil, p.membershipErr
	}
	return p.browserHostedProvider.Memberships(ctx, user, org)
}

func requireEntryAuthResponse(t *testing.T, status int, body string, want int) {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s want=%d", status, body, want)
	}
	if want == http.StatusServiceUnavailable && strings.Contains(body, "private-outage-sentinel") {
		t.Fatalf("dependency error leaked: %s", body)
	}
}

func TestPostgresEntryMembershipOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		for _, route := range []string{"admin", "context"} {
			for _, stage := range []string{"member", "credential", "authority"} {
				for _, fault := range []struct {
					name string
					err  error
					want int
				}{
					{"storage", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
					{"outage before denial", errors.Join(tenantauth.ErrUnavailable, accessstore.ErrDenied), http.StatusServiceUnavailable},
					{"outage before operator denial", errors.Join(tenantauth.ErrUnavailable, operatortool.ErrAccessDenied), http.StatusServiceUnavailable},
					{"missing", corestore.ErrNotFound, http.StatusForbidden},
					{"denied", accessstore.ErrDenied, http.StatusForbidden},
				} {
					t.Run(org+"/"+route+"/"+stage+"/"+fault.name, func(t *testing.T) {
						service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: stage, err: fault.err}
						defer func() { service.config.AccessRepository = repository }()
						response := f.request(t, org, "/internal/v1/keys/"+route, `{}`, nil)
						requireEntryAuthResponse(t, response.Code, response.Body.String(), fault.want)
					})
				}
			}
		}
		for _, control := range []struct {
			name, role      string
			active, revoked bool
		}{
			{"removed", "owner", false, false}, {"wrong role", "member", true, false}, {"revoked", "owner", true, true},
		} {
			t.Run(org+"/"+control.name, func(t *testing.T) {
				err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), org+"_owner")
					if err != nil {
						return err
					}
					member.Active, member.Role = control.active, control.role
					if err := w.UpdateAccessMember(t.Context(), member); err != nil {
						return err
					}
					if control.revoked {
						return w.RevokeAccessCredential(t.Context(), member.PrincipalID, time.Now())
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, route := range []string{"admin", "context"} {
					response := f.request(t, org, "/internal/v1/keys/"+route, `{}`, nil)
					want := http.StatusForbidden
					if control.name == "wrong role" && route == "context" {
						want = http.StatusOK
					}
					requireEntryAuthResponse(t, response.Code, response.Body.String(), want)
				}
			})
		}
	}
}

func TestPostgresOwnerBootstrapOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		provider := &entryAuthProvider{browserHostedProvider: &browserHostedProvider{members: map[string]auth.Membership{}}}
		member, err := provider.CreateMembership(t.Context(), org+"_owner", "provider_"+org, "owner")
		if err != nil {
			t.Fatal(err)
		}
		service.config.Hosted.Provider = provider
		for _, test := range []struct {
			name, stage, role string
			err               error
			want              int
		}{
			{"tenant outage", "tenant", "owner", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			{"provider outage", "provider", "owner", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			{"provider unavailable identity", "provider", "owner", &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, http.StatusServiceUnavailable},
			{"bootstrap write outage", "write", "owner", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			{"bootstrap outage before conflict", "write", "owner", errors.Join(tenantauth.ErrUnavailable, errHostedOwnerAlreadyBootstrapped), http.StatusServiceUnavailable},
			{"missing membership", "", "", nil, http.StatusForbidden},
			{"wrong role", "", "member", nil, http.StatusForbidden},
			{"owner", "", "owner", nil, http.StatusNoContent},
		} {
			t.Run(org+"/"+test.name, func(t *testing.T) {
				service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: test.stage, err: test.err}
				defer func() { service.config.AccessRepository = repository }()
				tenantRepository := service.config.TenantRepository
				if test.stage == "write" {
					service.config.TenantRepository = entryAuthFaultRepository{Repository: tenantRepository, stage: test.stage, err: test.err}
					defer func() { service.config.TenantRepository = tenantRepository }()
				}
				provider.membershipErr = nil
				if test.stage == "provider" {
					provider.membershipErr = test.err
				}
				member.Role.Slug = test.role
				provider.members[member.ID] = member
				response := f.request(t, org, "/internal/v1/owner/bootstrap", `{"organization_name":"Example"}`, func(c *cloudassert.Claims) { c.Subject = org + "_owner" })
				requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
			})
		}
	}
}
