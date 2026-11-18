package runnerenroll

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type enrollmentHTTPFixture struct {
	service *hubtest.HTTPHarness
	project tracker.NativeProject
	admin   hubtest.HTTPActor
}
type httpRunnerFixture struct {
	enrollmentHTTPFixture
	binding    runnerauth.Binding
	enrollment runnerauth.Enrollment
	redemption runnerauth.Redemption
	base       string
}

func newEnrollmentHTTP(t *testing.T, clock *hubtest.Clock) enrollmentHTTPFixture {
	t.Helper()
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: clock.Now()}).OrganizationID
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
	h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization, Now: clock.Now}, func(group *echo.Group, deps hubtest.HTTPDeps) {
		service := New(Deps{Repository: repository, Now: clock.Now, NewID: hubapi.NewID, GenerateToken: apikey.GenerateToken, TokenFingerprint: func(hash string) string { return hash[:12] }, WorkerScope: "worker",
			ApplyUsage:     func(context.Context, billingstore.Writer, time.Time, string, bool, bool) error { return nil },
			CheckAdmission: func(context.Context, billingstore.Writer, time.Time, int64, ...string) error { return nil },
			ValidPlatform:  func(string, string) bool { return true }, ValidSpriteSlug: func(string) bool { return true },
		})
		RegisterRoutes(group, HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Admin: deps.API(hubauth.Admin), Error: deps.Error, Invalid: func(c echo.Context, err error) error { return deps.Error(c, hubapi.Invalid(err.Error())) }})
	})
	return enrollmentHTTPFixture{service: h, project: project, admin: h.AdminKey(t)}
}
func prepareHTTPRunner(t *testing.T, f enrollmentHTTPFixture, operations ...string) httpRunnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", f.admin.Token, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: operations, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	return httpRunnerFixture{enrollmentHTTPFixture: f, binding: binding, enrollment: enrollment, base: base, redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "example-host", DisplayName: "Example runner", Capacity: 2, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}}}}
}
func (r *httpRunnerFixture) enroll(t *testing.T) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, r.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption), http.StatusCreated)
}
func performHubAPIRequest(t *testing.T, h *hubtest.HTTPHarness, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return h.Request(t, hubtest.HTTPActor{Token: token}, method, path, body).Recorder
}
func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
}
func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerEnrollmentScopeRevocationAndCollisions(t *testing.T) {
	t.Parallel()
	f := newEnrollmentHTTP(t, hubtest.NewClock(time.Now().UTC()))
	r := prepareHTTPRunner(t, f, runnerauth.Read)
	for _, test := range []struct {
		name   string
		change func(*runnerauth.EnrollmentRequest)
	}{
		{"no projects", func(r *runnerauth.EnrollmentRequest) { r.ProjectIDs = nil }},
		{"unknown project", func(r *runnerauth.EnrollmentRequest) {
			r.ProjectIDs = []tracker.ProjectID{tracker.ProjectID(hubapi.NewID("prj"))}
		}},
		{"duplicate projects", func(r *runnerauth.EnrollmentRequest) { r.ProjectIDs = append(r.ProjectIDs, r.ProjectIDs[0]) }},
		{"no operations", func(r *runnerauth.EnrollmentRequest) { r.Operations = nil }},
		{"administrator operation", func(r *runnerauth.EnrollmentRequest) { r.Operations = []string{"admin"} }},
		{"excessive TTL", func(r *runnerauth.EnrollmentRequest) { r.TTLSeconds = 901 }},
		{"zero TTL", func(r *runnerauth.EnrollmentRequest) { r.TTLSeconds = 0 }},
		{"mutable name ID", func(r *runnerauth.EnrollmentRequest) { r.MachineID = "hostname" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := runnerauth.EnrollmentRequest{Binding: runnerauth.NewBinding(), ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
			test.change(&request)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", f.admin.Token, request), http.StatusUnprocessableEntity)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.base+"/runner-enrollments/"+r.enrollment.ID, f.admin.Token, nil), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption), http.StatusUnauthorized)
	r = prepareHTTPRunner(t, f, runnerauth.Read)
	request := runnerauth.EnrollmentRequest{Binding: r.binding, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", f.admin.Token, request)
	requireNativeStatus(t, response, http.StatusCreated)
	var competing runnerauth.Enrollment
	decodeHubResponse(t, response, &competing)
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", competing.Token, r.redemption), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments", f.admin.Token, request), http.StatusConflict)
}

func TestRunnerEnrollmentValidationAndClock(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*httpRunnerFixture, *time.Time)
		want   int
	}{
		{"valid", func(*httpRunnerFixture, *time.Time) {}, http.StatusCreated},
		{"just before expiry", func(r *httpRunnerFixture, now *time.Time) { *now = r.enrollment.ExpiresAt.Add(-time.Nanosecond) }, http.StatusCreated},
		{"expiry boundary", func(r *httpRunnerFixture, now *time.Time) { *now = r.enrollment.ExpiresAt }, http.StatusUnauthorized},
		{"expired", func(r *httpRunnerFixture, now *time.Time) { *now = r.enrollment.ExpiresAt.Add(time.Second) }, http.StatusUnauthorized},
		{"clock before issuance", func(_ *httpRunnerFixture, now *time.Time) { *now = now.Add(-time.Nanosecond) }, http.StatusUnauthorized},
		{"invalid clock", func(_ *httpRunnerFixture, now *time.Time) { *now = time.Time{} }, http.StatusInternalServerError},
		{"wrong machine", func(r *httpRunnerFixture, _ *time.Time) { r.redemption.MachineID = runnerauth.NewBinding().MachineID }, http.StatusUnauthorized},
		{"wrong runner", func(r *httpRunnerFixture, _ *time.Time) { r.redemption.RunnerID = runnerauth.NewBinding().RunnerID }, http.StatusUnauthorized},
		{"wrong organization", func(r *httpRunnerFixture, _ *time.Time) { r.base = "/api/v2/organizations/" + hubapi.NewID("org") }, http.StatusUnauthorized},
		{"weak credential", func(r *httpRunnerFixture, _ *time.Time) { r.redemption.Credential = "example-provider-secret" }, http.StatusUnprocessableEntity},
		{"enrollment reused as identity", func(r *httpRunnerFixture, _ *time.Time) { r.redemption.Credential = r.enrollment.Token }, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			clock := hubtest.NewClock(now)
			f := newEnrollmentHTTP(t, clock)
			r := prepareHTTPRunner(t, f, runnerauth.Read)
			test.change(&r, &now)
			clock.Set(now)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption), test.want)
		})
	}
}
