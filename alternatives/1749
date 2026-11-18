package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func conversationWorkerRunner(t *testing.T, f nativeFixture) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, runnerauth.EnrollmentRequest{Scope: "projects", Binding: binding, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	runner := runnerFixture{nativeFixture: f, binding: binding, enrollment: enrollment, base: base, redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "fixture", DisplayName: "Example runner", Capacity: 1, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}}
	runner.enroll(t)
	return runner
}
