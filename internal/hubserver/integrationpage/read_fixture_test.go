package integrationpage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integrationpage"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type readDetails struct {
	*hubtest.Details
	beforeRead func(context.Context) error
}

func (d *readDetails) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if d.beforeRead != nil {
		if err := d.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	return d.Details.ReadDetail(ctx, org, ref)
}

type runnerReader interface {
	ReadRunnerAuthority(context.Context, tenantauth.RequestIdentity, func() time.Time) (tenantauth.Scope, int, error)
}
type repositorySwitch struct {
	runnerReader
	tenantstore.Repository
	tenantstore.RequestReader
}
type snapshotRepository struct {
	tenantstore.Repository
	tenantstore.RequestReader
	active atomic.Int32
}

func (r *snapshotRepository) ReadRequest(ctx context.Context, org tracker.OrganizationID, read func(context.Context) error) error {
	r.active.Add(1)
	defer r.active.Add(-1)
	return r.RequestReader.ReadRequest(ctx, org, read)
}

type revokingRepository struct {
	tenantstore.Repository
	tenantstore.RequestReader
	depth         atomic.Int32
	afterSnapshot func(context.Context) error
}

func (r *revokingRepository) ReadRequest(ctx context.Context, org tracker.OrganizationID, read func(context.Context) error) error {
	r.depth.Add(1)
	err := r.RequestReader.ReadRequest(ctx, org, read)
	if r.depth.Add(-1) == 0 && err == nil && r.afterSnapshot != nil {
		revoke := r.afterSnapshot
		r.afterSnapshot = nil
		err = revoke(ctx)
	}
	return err
}

type selectedTenant struct{ hubmaintenance.Tenant }

func (r selectedTenant) Page(ctx context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || r.Organization <= after {
		return nil, nil
	}
	return []hubmaintenance.Tenant{r.Tenant}, nil
}

type readFixture struct {
	admin      *sql.DB
	original   *postgres.Repository
	repository *repositorySwitch
	details    *readDetails
	h          *hubtest.HTTPHarness
	live       atomic.Int32
}

func newReadFixture(t *testing.T) *readFixture {
	t.Helper()
	admin := hubtest.Database(t)
	original := hubtest.RepositoryOn(t, admin)
	f := &readFixture{admin: admin, original: original, details: &readDetails{Details: hubtest.NewDetails()}, repository: &repositorySwitch{runnerReader: original, Repository: original, RequestReader: original}}
	if err := original.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_first", Name: "Example", Slug: "example", Local: false, CreatedAt: time.Now()}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_first", ProviderID: "provider_org_first", Deployment: "origin", PublicURL: "https://hub.example.test"})
	}); err != nil {
		t.Fatal(err)
	}
	f.h = hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: f.repository, Organization: "org_first"}, func(g *echo.Group, deps hubtest.HTTPDeps) {
		g.GET("/api/v2/organizations/:organization/projects/:project/integration", func(c echo.Context) error { return f.pageService().Get(c) }, deps.API(hubauth.Worker, hubauth.Operator))
	})
	return f
}
func (f *readFixture) use(repository tenantstore.Repository) {
	f.repository.Repository = repository
	if authority, ok := repository.(runnerReader); ok {
		f.repository.runnerReader = authority
	} else {
		f.repository.runnerReader = f.original
	}
	f.repository.RequestReader = repository.(tenantstore.RequestReader)
}

type readProject struct {
	project tracker.NativeProject
	base    string
	actor   hubtest.HTTPActor
}

func (f *readFixture) project(t *testing.T, org tracker.OrganizationID) readProject {
	t.Helper()
	if org != "org_first" {
		hubtest.Organization(t, f.original, hubtest.OrganizationOptions{ID: org})
	}
	project := hubtest.Project(t, f.original, org, hubtest.ProjectOptions{Name: "Example"})
	credential := hubtest.Credential(t, f.original, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	return readProject{project: project, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID), actor: hubtest.HTTPActor{Token: credential.Token, Credential: credential}}
}
func (f *readFixture) pageService() *integrationpage.Service {
	readProject := func(ctx context.Context, scope hubapi.Scope) (projectstore.Record, error) {
		var record projectstore.Record
		err := f.repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			record, err = r.ProjectRecord(ctx, scope.Project)
			return err
		})
		return record, hubauth.RecordError(err)
	}
	return integrationpage.New(integrationpage.Deps{Repository: f.repository, Details: hubmaintenance.LimitDetails(f.details), Now: time.Now, RecordError: hubauth.RecordError,
		ReadProject: readProject, Scope: func(c echo.Context) hubapi.Scope { return c.Get("hub_request_scope").(hubapi.Scope) }, Error: hubtest.HTTPError, Audit: func(echo.Context, hubapi.Scope) error { return nil }, Live: func(echo.Context, *projectstore.Integration) error { f.live.Add(1); return nil },
		Workflow: func(ctx context.Context, record projectstore.Record) (string, error) {
			if record.Settings.WorkflowKey == "" {
				return "", nil
			}
			v := record.Settings
			raw, err := eventstore.NewContent(hubmaintenance.LimitDetails(f.details)).Receipt(ctx, eventstore.Receipt{OrganizationID: record.Core.OrganizationID, PayloadObjectKey: v.WorkflowKey, PayloadSHA256: v.WorkflowSHA256, PayloadBytes: v.WorkflowBytes, PayloadContentBytes: v.WorkflowContentBytes})
			if err != nil {
				return "", err
			}
			var markdown string
			err = json.Unmarshal(raw, &markdown)
			return markdown, err
		},
		Resolve: func(ctx context.Context, c echo.Context) error {
			a := f.h.Deps.Auth
			return a.ResolveNative(ctx, c, hubauth.NativeOptions{Credential: hubauth.RequestCredential, EventScope: func(echo.Context, hubauth.Input) bool { return false }, ArtifactRead: func(echo.Context) bool { return false }, RelayTicket: func(echo.Context) bool { return false }, LeaseRoute: func(string) bool { return false }, RepositoryRoute: func(string) bool { return false }, RecordError: hubauth.RecordError,
				ReadProject: func(ctx context.Context, input hubauth.Input, write, models bool) error {
					return f.repository.ReadTenant(ctx, input.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						if err := a.Authorize(ctx, r, input, write); err != nil {
							return err
						}
						_, err := r.ProjectRecord(ctx, input.Project)
						return hubauth.RecordError(err)
					})
				},
				Store: func(c echo.Context, input hubauth.Input) { c.Set("hub_request_scope", a.Scope(input)) },
			})
		},
	})
}
func (f *readFixture) request(t *testing.T, project readProject) *httptest.ResponseRecorder {
	t.Helper()
	response := f.h.Request(t, project.actor, http.MethodGet, project.base+"/integration", nil)
	return response.Recorder
}
func requireStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
}
func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}
