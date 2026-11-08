package hubserver

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
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
				switch stage {
				case "slug":
					path = "/work"
				case "readable member":
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

func TestPostgresAppReservedRoutesReturnNotFound(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org := range f.services {
		for _, path := range []string{"/api/v2/unknown", "/api", "/auth/unknown", "/webhooks/unknown", "/app/unknown", "/logout", "/metrics"} {
			t.Run(org+path, func(t *testing.T) {
				response := f.request(t, org, "/organizations/"+org+path, "", func(c *cloudassert.Claims) {
					sharedRouteBrowserClaims(c)
					c.Method = http.MethodGet
				})
				requireEntryAuthResponse(t, response.Code, response.Body.String(), http.StatusNotFound)
			})
		}
	}
}

func TestPostgresOrganizationPageUsesGrantedProjects(t *testing.T) {
	f := newPostgresBrowserHostedFixture(t, true)
	for _, test := range []struct {
		account string
		private bool
	}{{account: "owner", private: true}, {account: "viewer"}} {
		t.Run(test.account, func(t *testing.T) {
			response := f.appRequest(t, test.account, http.MethodGet, "/organization")
			browserHostedStatus(t, response, http.StatusOK)
			body := response.Body.String()
			if !strings.Contains(body, "Browser collaboration") || strings.Contains(body, "Owner private project") != test.private {
				t.Fatalf("organization page lost project authorization: %s", body)
			}
		})
	}
}

func TestPostgresAppAssetsUseDeclaredFiles(t *testing.T) {
	useAppClientFS(t, fstest.MapFS{})
	type fixture struct {
		organization string
		browser      *browserHostedFixture
		script       string
		missing      bool
	}
	var fixtures []fixture
	for _, organization := range []string{"org_first", "org_second", "org_missing"} {
		files := fstest.MapFS{}
		script := "console.log('" + organization + "');"
		missing := organization == "org_missing"
		if !missing {
			body := strings.Replace(appShellTestBody, `<div id="root"></div>`, `<div id="root">`+organization+`</div>`, 1)
			files[conversationClientShell] = &fstest.MapFile{Data: []byte(body)}
			files[conversationClientEntry] = &fstest.MapFile{Data: []byte(script)}
		}
		browser := newPostgresBrowserHostedOrganizationFixture(t, true, organization, func(cfg *Config) { cfg.StaticFS = files })
		fixtures = append(fixtures, fixture{organization, browser, script, missing})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.organization, func(t *testing.T) {
			response := fixture.browser.appRequest(t, "owner", http.MethodGet, "/settings/billing")
			if fixture.missing {
				if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"client_unavailable"`) {
					t.Fatalf("missing declared client status=%d", response.Code)
				}
				return
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `<div id="root">`+fixture.organization+`</div>`) {
				t.Errorf("shell ignored instance files: status=%d", response.Code)
			}
			if fixture.browser.service.clientBuild.Build != sha256Hex(fixture.script) {
				t.Error("client identity ignored instance files")
			}
			if !strings.HasSuffix(fixture.browser.service.hostedBillingReturn(false), "/settings/billing") {
				t.Error("billing return ignored declared client")
			}
			asset := fixture.browser.appRequest(t, "", http.MethodGet, "/static/"+conversationClientEntry)
			if asset.Code != http.StatusOK || asset.Body.String() != fixture.script {
				t.Errorf("static asset ignored instance files: status=%d bytes=%d", asset.Code, asset.Body.Len())
			}
		})
	}
}
