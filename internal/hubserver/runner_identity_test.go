package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerFixture struct {
	nativeFixture
	binding    runnerauth.Binding
	enrollment runnerauth.Enrollment
	redemption runnerauth.Redemption
	identity   runnerauth.Identity
	base       string
}

func prepareRunner(t *testing.T, f nativeFixture, operations ...string) runnerFixture {
	t.Helper()
	return prepareRunnerForScope(t, f, "projects", operations...)
}

func prepareRunnerForScope(t *testing.T, f nativeFixture, scope string, operations ...string) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	projects := []tracker.ProjectID{f.project.ID}
	if scope == "organization" {
		projects = nil
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, runnerauth.EnrollmentRequest{Scope: scope, Binding: binding, ProjectIDs: projects, Operations: operations, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	return runnerFixture{nativeFixture: f, binding: binding, enrollment: enrollment, base: base,
		redemption: runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}}
}

func (r *runnerFixture) enroll(t *testing.T) {
	t.Helper()
	response := performHubAPIRequest(t, r.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption)
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &r.identity)
}

func (r runnerFixture) identityPath() string { return r.base + "/runners/" + r.binding.RunnerID }

func (r runnerFixture) reportCapacity(t *testing.T) {
	t.Helper()
	reportRunnerCapacity(t, r.service, r.nativeFixture.base, r.redemption.Credential, r.binding.MachineID, r.redemption.DisplayName, r.redemption.BackendIsolation, r.redemption.Capacity)
}
