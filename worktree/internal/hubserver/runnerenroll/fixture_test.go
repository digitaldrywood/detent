package runnerenroll

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type enrollmentFixture struct {
	service    *Service
	repository tenantstore.Repository
	project    tracker.NativeProject
	scope      hubapi.Scope
}

func enrollmentFixtures(t *testing.T) []enrollmentFixture {
	t.Helper()
	repository := hubtest.Repository(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := New(Deps{
		Repository:       repository,
		Now:              func() time.Time { return now },
		NewID:            hubapi.NewID,
		GenerateToken:    apikey.GenerateToken,
		TokenFingerprint: func(hash string) string { return hash[:12] },
		WorkerScope:      "worker",
		ApplyUsage:       func(context.Context, billingstore.Writer, time.Time, string, bool, bool) error { return nil },
		CheckAdmission:   func(context.Context, billingstore.Writer, time.Time, int64, ...string) error { return nil },
		ValidPlatform:    func(string, string) bool { return true },
		ValidSpriteSlug:  func(slug string) bool { return strings.ToLower(slug) == slug },
	})
	var fixtures []enrollmentFixture
	for range 2 {
		organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
		project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
		credential := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Scope: "admin", AllProject: true, Now: now})
		scope := hubtest.Scope(t, hubtest.ScopeFacts{Credential: credential, Admin: true, Now: service.deps.Now})
		fixtures = append(fixtures, enrollmentFixture{service: service, repository: repository, project: project, scope: scope})
	}
	return fixtures
}

type runnerFixture struct {
	enrollmentFixture
	binding    runnerauth.Binding
	enrollment runnerauth.Enrollment
	redemption runnerauth.Redemption
	identity   runnerauth.Identity
}

func prepareRunner(t *testing.T, f enrollmentFixture, operations ...string) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	value, err := f.service.Create(t.Context(), f.scope, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: operations, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	return runnerFixture{enrollmentFixture: f, binding: binding, enrollment: value.(runnerauth.Enrollment), redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "example-host", DisplayName: "Example runner", Capacity: 2, Version: "test"}}
}

func (r *runnerFixture) enroll(t *testing.T) {
	t.Helper()
	var err error
	r.identity, err = r.service.Redeem(t.Context(), r.project.OrganizationID, r.enrollment.Token, r.redemption)
	if err != nil {
		t.Fatal(err)
	}
}

func status(err error, success int) int {
	if err == nil {
		return success
	}
	var failure *hubapi.Error
	if errors.As(err, &failure) {
		return failure.Status
	}
	return http.StatusInternalServerError
}

func requireStatus(t *testing.T, err error, success, want int) {
	t.Helper()
	if got := status(err, success); got != want {
		t.Fatalf("status = %d, want %d: %v", got, want, err)
	}
}
