package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func hostedOutageRequest(t *testing.T, f sharedRouteFixture, org, method, path, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	id, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := cloudassert.Claims{Issuer: "entry", Audience: org, Email: org + "_owner@example.test", Subject: org + "_owner", Role: "owner", Generation: 1, Method: method, Path: path, BodyDigest: cloudassert.BodyDigest([]byte(body)), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id}
	sharedRouteBrowserClaims(&claims)
	assertion, err := cloudassert.Sign(f.key, claims)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-CSRF-Token", claims.CSRF)
	request.Header.Set(cloudassert.Header, assertion)
	if strings.HasSuffix(path, "/mcp") {
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", "tools/call")
		request.Header.Set("Mcp-Name", "monthly_usage_costs")
	}
	response := httptest.NewRecorder()
	f.services[org].Handler().ServeHTTP(response, request)
	return response
}

type hostedUsageFaultRepository struct {
	tenantstore.Repository
	err error
}

func (r hostedUsageFaultRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(hostedUsageFaultReader{Reader: reader, err: r.err})
	})
}

type hostedUsageFaultReader struct {
	tenantstore.Reader
	err error
}

func (r hostedUsageFaultReader) AccessMissingUsageGrant(context.Context, string) (bool, error) {
	return false, r.err
}

func TestPostgresHostedEndpointCredentialOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		base := "/api/v2/organizations/" + org
		for _, route := range []struct{ method, path, body string }{
			{http.MethodDelete, base + "/members/missing", ""},
			{http.MethodPut, base + "/members/missing/role", `{"role":"member"}`},
			{http.MethodPost, base + "/members/invitations", `{"email":"member@example.test","role":"member","idempotency_key":"example"}`},
			{http.MethodPut, base + "/members/invitations/missing", `{}`},
			{http.MethodPut, base + "/members/missing/grants", `{}`},
			{http.MethodDelete, base + "/members/invitations/missing", ""},
			{http.MethodPost, base + "/members/invitations/missing/resend", `{}`},
			{http.MethodGet, base + "/api-keys", ""},
			{http.MethodPost, base + "/api-keys", `{}`},
			{http.MethodDelete, base + "/api-keys/missing", ""},
			{http.MethodPost, "/organizations/" + org + "/organization/members/missing/revoke", ""},
			{http.MethodPost, "/organizations/" + org + "/organization/members/missing/role", ""},
			{http.MethodPost, "/organizations/" + org + "/organization/grants", ""},
			{http.MethodPost, "/organizations/" + org + "/organization/invite", ""},
			{http.MethodPost, "/organizations/" + org + "/projects", ""},
		} {
			for _, fault := range []struct {
				name string
				err  error
				want int
			}{
				{"storage", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"outage before denial", errors.Join(tenantauth.ErrUnavailable, accessstore.ErrDenied), http.StatusServiceUnavailable},
				{"missing", corestore.ErrNotFound, http.StatusForbidden},
			} {
				t.Run(org+"/"+route.method+route.path+"/"+fault.name, func(t *testing.T) {
					service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: "member", err: fault.err}
					defer func() { service.config.AccessRepository = repository }()
					response := hostedOutageRequest(t, f, org, route.method, route.path, route.body, "application/json")
					requireEntryAuthResponse(t, response.Code, response.Body.String(), fault.want)
				})
			}
		}
	}
}

func TestPostgresHostedMemberProviderOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		provider := &entryAuthProvider{browserHostedProvider: &browserHostedProvider{members: map[string]auth.Membership{}}}
		service.config.Hosted.Provider = provider
		base := "/api/v2/organizations/" + org + "/members/missing"
		for _, route := range []struct{ method, path, body, contentType string }{
			{http.MethodDelete, base, "", "application/json"},
			{http.MethodPut, base + "/role", `{"role":"member"}`, "application/json"},
			{http.MethodPut, base + "/grants", `{"project_id":"project_first"}`, "application/json"},
			{http.MethodPost, "/organizations/" + org + "/organization/members/missing/revoke", "", "application/x-www-form-urlencoded"},
			{http.MethodPost, "/organizations/" + org + "/organization/members/missing/role", "role=member", "application/x-www-form-urlencoded"},
		} {
			for _, fault := range []struct {
				name string
				err  error
				want int
			}{
				{"provider", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"provider unavailable identity", &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, http.StatusServiceUnavailable},
				{"missing", nil, http.StatusForbidden},
			} {
				t.Run(org+"/"+route.method+route.path+"/"+fault.name, func(t *testing.T) {
					provider.membershipErr = fault.err
					response := hostedOutageRequest(t, f, org, route.method, route.path, route.body, route.contentType)
					want := fault.want
					if strings.HasSuffix(route.path, "/grants") && fault.err == nil {
						want = http.StatusNotFound
					}
					requireEntryAuthResponse(t, response.Code, response.Body.String(), want)
				})
			}
		}
	}
}
