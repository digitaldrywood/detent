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

func (r hostedUsageFaultReader) AccessMissingUsageGrant(context.Context, string, string) (bool, error) {
	return false, r.err
}

func TestPostgresHostedMemberProviderOutages(t *testing.T) {
	t.Parallel()
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		t.Run(org+"/browser membership lookup", func(t *testing.T) {
			repository := service.config.AccessRepository
			defer func() { service.config.AccessRepository = repository }()
			path := "/api/v2/organizations/" + org + "/api-keys"
			for _, test := range []struct {
				err  error
				want int
			}{
				{errors.Join(errors.New("private-outage-sentinel"), tenantauth.ErrUnavailable, accessstore.ErrDenied), http.StatusServiceUnavailable},
				{corestore.ErrNotFound, http.StatusForbidden},
			} {
				service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: "member", err: test.err}
				response := hostedOutageRequest(t, f, org, http.MethodGet, path, "", "application/json")
				requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
			}
		})
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
