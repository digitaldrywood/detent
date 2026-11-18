package workitemchange_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptDiffFixture struct {
	hubfixture.Native
	worker   string
	runnerID string
	issue    tracker.NativeIssue
	policy   string
	lease    tracker.NativeLease
	attempt  string
	run      string
}

func newAttemptDiffFixture(t *testing.T) *attemptDiffFixture {
	t.Helper()
	f := newClaimedAttemptDiffFixture(t)
	f.start(t)
	return f
}

func newClaimedAttemptDiffFixture(t *testing.T) *attemptDiffFixture {
	t.Helper()
	base := hubfixture.NewNativeInOrganization(t, nil)
	f := &attemptDiffFixture{Native: base, policy: hubtest.PolicyDescriptor().ID}
	f.issue = base.Create(t, "diff-work")
	binding := runnerauth.NewBinding()
	operations := []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}
	orgBase := "/api/v2/organizations/" + string(f.Project.OrganizationID)
	response := hubfixture.Request(t, f.Hub.Service.Handler(), http.MethodPost, orgBase+"/runner-enrollments", f.Hub.AdminToken(), runnerauth.EnrollmentRequest{Scope: "projects", Binding: binding, ProjectIDs: []tracker.ProjectID{f.Project.ID}, Operations: operations, TTLSeconds: 60})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	hubfixture.Decode(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	backends := isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}
	redemption := runnerauth.Redemption{BackendIsolation: backends, Binding: binding, Credential: credential, Hostname: "example-host", DisplayName: "Runner", Capacity: 2, Version: "test"}
	response = hubfixture.Request(t, f.Hub.Service.Handler(), http.MethodPost, orgBase+"/runner-enrollments/redeem", enrollment.Token, redemption)
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	response = hubfixture.Request(t, f.Hub.Service.Handler(), http.MethodPost, f.Base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": backends, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: time.Now()}})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	f.worker, f.runnerID = credential, binding.RunnerID
	response = hubfixture.Request(t, f.Hub.Service.Handler(), http.MethodPost, f.Base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &f.lease)
	f.attempt, f.run = hubapi.NewID("attempt"), hubapi.NewID("run")
	return f
}

func (f *attemptDiffFixture) start(t *testing.T) {
	t.Helper()
	f.event(t, "run.started", 1, "")
}

func (f *attemptDiffFixture) event(t *testing.T, kind string, sequence int64, outcome string) {
	t.Helper()
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("event")}, Type: kind, SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: sequence, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: outcome}}
	hubfixture.RequireStatus(t, hubfixture.Request(t, f.Hub.Service.Handler(), http.MethodPost, f.Base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
}

func (f *attemptDiffFixture) request(seq int64, files ...tracker.AttemptDiffFile) tracker.AttemptDiffRequest {
	return tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: f.attempt, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: seq}, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Files: files}
}

func (f *attemptDiffFixture) post(t *testing.T, request tracker.AttemptDiffRequest) *httptest.ResponseRecorder {
	t.Helper()
	return hubfixture.Request(t, f.Hub.Service.Handler(), http.MethodPost, f.Base+"/attempts/"+f.attempt+"/diff", f.worker, request)
}

func requireNativeCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) hubapi.Error {
	t.Helper()
	hubfixture.RequireStatus(t, response, status)
	var failure hubapi.Error
	hubfixture.Decode(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
	return failure
}
