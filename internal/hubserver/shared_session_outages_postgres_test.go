package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r entryAuthFaultReader) AccessSession(ctx context.Context, hash string) (accessstore.Session, error) {
	if r.stage == "session" {
		return accessstore.Session{}, r.err
	}
	return r.Reader.AccessSession(ctx, hash)
}

func (r entryAuthFaultReader) AccessOrganization(ctx context.Context) (accessstore.Organization, error) {
	if r.stage == "slug" {
		return accessstore.Organization{}, r.err
	}
	return r.Reader.AccessOrganization(ctx)
}

type runnerGrantOutageRepository struct {
	tenantstore.Repository
	err error
}

type entryPrincipalOutageRepository struct {
	tenantstore.Repository
	err error
}

func (r entryPrincipalOutageRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
		return write(entryPrincipalOutageWriter{Writer: w, err: r.err})
	})
}

type entryPrincipalOutageWriter struct {
	tenantstore.Writer
	err error
}

func (w entryPrincipalOutageWriter) EnsureAccessCredential(context.Context, accessstore.Credential) error {
	return w.err
}

func (r runnerGrantOutageRepository) ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error {
	return r.err
}

func TestPostgresSharedSessionOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		for _, path := range []string{"/app/bootstrap", "/app/updates"} {
			for _, test := range []struct {
				name, stage string
				err         error
				want        int
			}{
				{"session read", "session", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"session read before denial", "session", errors.Join(tenantauth.ErrUnavailable, auth.ErrInvalidSession), http.StatusServiceUnavailable},
				{"session read before missing", "session", errors.Join(tenantauth.ErrUnavailable, corestore.ErrNotFound), http.StatusServiceUnavailable},
				{"session write", "write", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"membership read", "member", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"missing member", "member", corestore.ErrNotFound, http.StatusForbidden},
				{"invalid session", "session", auth.ErrInvalidSession, http.StatusUnauthorized},
			} {
				t.Run(org+path+"/"+test.name, func(t *testing.T) {
					service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: test.stage, err: test.err}
					defer func() { service.config.AccessRepository = repository }()
					response := f.request(t, org, "/organizations/"+org+path, "", func(c *cloudassert.Claims) {
						sharedRouteBrowserClaims(c)
						c.Method = http.MethodGet
						if test.stage == "write" {
							c.Binding = strings.Repeat("d", 64)
						}
					})
					requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
				})
			}
		}
	}
}

func TestPostgresEntryCredentialOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"example-client","version":"1"}}}`
	for org, service := range f.services {
		repository := service.config.AccessRepository
		for _, kind := range []string{"personal", "service"} {
			for _, test := range []struct {
				name, stage string
				err         error
				want        int
			}{
				{"credential read", "credential", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"outage before denial", "credential", errors.Join(tenantauth.ErrUnavailable, accessstore.ErrDenied), http.StatusServiceUnavailable},
				{"missing credential", "credential", corestore.ErrNotFound, http.StatusForbidden},
				{"denied credential", "credential", accessstore.ErrDenied, http.StatusForbidden},
				{"service principal write", "write", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			} {
				if test.stage == "write" && kind != "service" {
					continue
				}
				t.Run(org+"/"+kind+"/"+test.name, func(t *testing.T) {
					service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: test.stage, err: test.err}
					if test.stage == "write" {
						service.config.AccessRepository = entryPrincipalOutageRepository{Repository: repository, err: test.err}
					}
					defer func() { service.config.AccessRepository = repository }()
					response := f.request(t, org, "/organizations/"+org+"/mcp", body, func(c *cloudassert.Claims) {
						c.Kind = cloudassert.KindKey
						c.ProviderOrganization = "provider_" + org
						c.Key = &apikey.KeyAuthority{ID: "key_" + org + "_" + strings.ReplaceAll(test.name, " ", "_"), CredentialBinding: strings.Repeat("c", 64), Kind: kind, Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}
					})
					requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
				})
			}
		}
	}
}
