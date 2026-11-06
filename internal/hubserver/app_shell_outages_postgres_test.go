package hubserver

import (
	"errors"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"net/http"
	"testing"
	"time"
)

func TestPostgresAppShellAuthorityOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		for _, stage := range []string{"session", "member", "project", "slug", "readable member"} {
			t.Run(org+"/"+stage, func(t *testing.T) {
				faultStage := stage
				if stage == "readable member" {
					faultStage = "member"
				}
				service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: faultStage, err: errors.New("private-outage-sentinel")}
				defer func() { service.config.AccessRepository = repository }()
				if stage == "project" {
					tenantRepository := service.config.TenantRepository
					service.config.TenantRepository = runnerGrantOutageRepository{Repository: tenantRepository, err: errors.New("private-outage-sentinel")}
					defer func() { service.config.TenantRepository = tenantRepository }()
				}
				path := "/projects/missing"
				if stage == "slug" {
					path = "/work"
				} else if stage == "readable member" {
					path = "/example"
				}
				response := f.request(t, org, "/organizations/"+org+path, "", func(c *cloudassert.Claims) {
					sharedRouteBrowserClaims(c)
					c.Method = http.MethodGet
				})
				requireEntryAuthResponse(t, response.Code, response.Body.String(), http.StatusServiceUnavailable)
			})
		}
		response := f.request(t, org, "/organizations/"+org+"/projects/missing", "", func(c *cloudassert.Claims) {
			sharedRouteBrowserClaims(c)
			c.Method = http.MethodGet
		})
		requireEntryAuthResponse(t, response.Code, response.Body.String(), http.StatusNotFound)
	}
}

func TestPostgresAppRunnerGrantOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.TenantRepository
		for _, fault := range []error{errors.New("private-outage-sentinel"), errors.Join(tenantauth.ErrUnavailable, accessstore.ErrDenied)} {
			t.Run(org+"/"+fault.Error(), func(t *testing.T) {
				service.config.TenantRepository = runnerGrantOutageRepository{Repository: repository, err: fault}
				defer func() { service.config.TenantRepository = repository }()
				response := f.request(t, org, "/organizations/"+org+"/app/updates", "", func(c *cloudassert.Claims) {
					sharedRouteBrowserClaims(c)
					c.Method, c.Role = http.MethodGet, "member"
					c.SessionCreatedAt = time.Now().Add(-time.Minute)
				})
				requireEntryAuthResponse(t, response.Code, response.Body.String(), http.StatusServiceUnavailable)
			})
		}
		response := f.request(t, org, "/organizations/"+org+"/app/updates", "", func(c *cloudassert.Claims) {
			sharedRouteBrowserClaims(c)
			c.Method, c.Role = http.MethodGet, "viewer"
		})
		requireEntryAuthResponse(t, response.Code, response.Body.String(), http.StatusNotFound)
	}
}
