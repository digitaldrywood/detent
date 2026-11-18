package hubfixture

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Runner struct {
	Native
	Binding    runnerauth.Binding
	Enrollment runnerauth.Enrollment
	Redemption runnerauth.Redemption
	Identity   runnerauth.Identity
	OrgBase    string
}

func PrepareRunner(t *testing.T, p Native, operations ...string) Runner {
	t.Helper()
	return PrepareRunnerForScope(t, p, "projects", operations...)
}

func PrepareRunnerForScope(t *testing.T, p Native, scope string, operations ...string) Runner {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := OrganizationBase(p.Project.OrganizationID)
	projects := []tracker.ProjectID{p.Project.ID}
	if scope == "organization" {
		projects = nil
	}
	response := p.Hub.Request(t, http.MethodPost, base+"/runner-enrollments", string(p.Hub.Config.InitialAdminToken), runnerauth.EnrollmentRequest{Scope: scope, Binding: binding, ProjectIDs: projects, Operations: operations, TTLSeconds: 60})
	RequireStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	Decode(t, response, &enrollment)
	return Runner{Native: p, Binding: binding, Enrollment: enrollment, OrgBase: base,
		Redemption: runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}}
}

func (r *Runner) Enroll(t *testing.T) {
	t.Helper()
	response := r.Hub.Request(t, http.MethodPost, r.OrgBase+"/runner-enrollments/redeem", r.Enrollment.Token, r.Redemption)
	RequireStatus(t, response, http.StatusCreated)
	Decode(t, response, &r.Identity)
}

func (r Runner) IdentityPath() string { return r.OrgBase + "/runners/" + r.Binding.RunnerID }

func (r Runner) ReportCapacity(t *testing.T) {
	t.Helper()
	ReportRunnerCapacity(t, r.Hub, r.Base, r.Redemption.Credential, r.Binding.MachineID, r.Redemption.DisplayName, r.Redemption.BackendIsolation, r.Redemption.Capacity)
}

func ReportRunnerCapacity(t *testing.T, h *Hub, projectBase, credential string, machine tracker.MachineID, displayName string, backends isolation.Report, capacity int) {
	t.Helper()
	RequireStatus(t, h.Request(t, http.MethodPost, projectBase+"/machines/"+string(machine)+"/heartbeat", credential, map[string]any{
		"display_name": displayName, "capacity": capacity, "version": "test", "backend_isolation": backends,
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: h.Now()},
	}), http.StatusOK)
}

func CapacityReport(now time.Time) providercapacity.Report {
	return providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local", SharedAccountAlias: "team", Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
}

func PublishCapacity(t *testing.T, p Native, r Runner, reports ...providercapacity.Report) {
	t.Helper()
	response := p.Hub.Request(t, http.MethodPost, p.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential,
		map[string]any{"backend_isolation": r.Redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": reports,
			"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: p.Hub.Now()}})
	RequireStatus(t, response, http.StatusOK)
}

func ProviderClaim(r Runner, issue tracker.NativeIssue, session string) tracker.NativeClaim {
	return tracker.NativeClaim{PolicyID: Policy().ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: session, TTLSeconds: 90,
		ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: "implement", Backend: "codex", Model: "test-model"}}}}
}

func StartedEvent(lease tracker.NativeLease) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt")}}
}

func Checkpoint() *tracker.NativeCheckpoint {
	return &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
}

func SharedRunner(t *testing.T, first Runner) Runner {
	t.Helper()
	binding := runnerauth.NewBinding()
	binding.MachineID = first.Binding.MachineID
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	r := Runner{Native: first.Native, Binding: binding, OrgBase: first.OrgBase,
		Redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "shared", DisplayName: "Second", Capacity: 8, Version: "test", OS: "linux", Architecture: "arm64"}}
	response := r.Hub.Request(t, http.MethodPost, r.OrgBase+"/runner-enrollments", string(r.Hub.Config.InitialAdminToken),
		runnerauth.EnrollmentRequest{Binding: binding, SharedMachine: true, ProjectIDs: []tracker.ProjectID{r.Project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	RequireStatus(t, response, http.StatusCreated)
	Decode(t, response, &r.Enrollment)
	r.Enroll(t)
	return r
}
