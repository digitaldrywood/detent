package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *browserHostedFixture) seedWorkflowRevisions(t *testing.T) {
	t.Helper()
	base := browserHostedOrganizationBase + "/projects/" + f.project
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = ? WHERE id = ?", "acme/orders", f.project); err != nil {
		t.Fatal(err)
	}
	binding := runnerauth.NewBinding()
	request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 900}
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments", request, http.StatusCreated), &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "test-host", DisplayName: "Workflow runner", Capacity: 2, Version: "test", OS: "linux", Architecture: "arm64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	original := hubTestPolicy()
	original.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("a", 40), States: append(nativeFixtureStates(), policy.State{Name: "Archive", Terminal: true})}
	original = original.WithID()
	f.api(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: original}, http.StatusOK)
	applied := original
	applied.SourceDigest = policy.Digest([]byte("applied workflow"))
	applied.Gates.AutoPromote = true
	applied.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("b", 40), States: []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "Review", Transitions: []string{"Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Review"}},
		{Name: "Done", Terminal: true},
	}}
	applied = applied.WithID()
	source := &policy.RepositorySource{Repository: "acme/orders", Commit: applied.Workflow.Revision, DefaultBranch: "develop", DefaultBranchHead: applied.Workflow.Revision, DefaultBranchReachable: true}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/policy/observed", credential, policy.Observation{Descriptor: applied, Source: source}), http.StatusNoContent)
	pending := applied
	pending.SourceDigest = policy.Digest([]byte("pending workflow"))
	pending.Gates.AutoPromote = false
	pending.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("c", 40), States: []policy.State{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Review"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Review", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}}
	pending = pending.WithID()
	pendingSource := *source
	pendingSource.Commit, pendingSource.DefaultBranchReachable = pending.Workflow.Revision, false
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/policy/observed", credential, policy.Observation{Descriptor: pending, Source: &pendingSource}), http.StatusNoContent)
}
