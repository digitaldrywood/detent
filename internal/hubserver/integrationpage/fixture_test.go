package integrationpage

import (
	"context"
	"database/sql"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"testing"
	"time"
)

func TestMain(m *testing.M) { hubtest.Main(m) }
func pageFixture(t *testing.T) (*Service, tenantstore.Repository, *sql.DB) {
	t.Helper()
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	service := New(Deps{Repository: repository, Details: hubmaintenance.LimitDetails(hubtest.NewDetails()), Now: time.Now, Workflow: func(context.Context, projectstore.Record) (string, error) { return "", nil }})
	return service, repository, admin
}
func pageProject(t *testing.T, repository tenantstore.Repository, org tracker.OrganizationID, name string) tracker.NativeProject {
	t.Helper()
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: org})
	return hubtest.Project(t, repository, org, hubtest.ProjectOptions{Name: name})
}
