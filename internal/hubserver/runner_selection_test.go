package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerAllowedProviderEligibility(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name        string
		unavailable int
		ready       bool
	}{
		{"unavailable project yields to ready project", 1, false},
		{"ready work beyond first candidate page wins", 100, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			preferred := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "preferred-provider")
			general := newNativeFixture(t, preferred.service, preferred.project.OrganizationID, "general-provider")
			r := prepareRunner(t, preferred, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Provider runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{preferred.project.ID, general.project.ID}}}
			requireNativeStatus(t, performHubAPIRequest(t, preferred.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			for _, f := range []nativeFixture{preferred, general} {
				approveHubTestPolicy(t, preferred.service, f.base+"/policy", hubTestPolicy())
			}
			for i := range scenario.unavailable {
				preferred.create(t, fmt.Sprintf("unavailable model %d", i))
			}
			var preferredIssue tracker.NativeIssue
			if scenario.ready {
				preferredIssue = preferred.create(t, "ready after unavailable candidates")
				if _, err := preferred.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_created_at = ? WHERE native_id = ?", formatHubTime(now.Add(time.Second)), preferredIssue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			generalIssue := general.create(t, "available model")
			publishCapacity(t, preferred, r, capacityReport(now))
			claim := providerClaim(r, generalIssue, "general-provider")
			if scenario.ready {
				claim.ProviderCandidates = append(claim.ProviderCandidates, providerClaim(r, preferredIssue, "").ProviderCandidates[0])
			}
			response := performHubAPIRequest(t, preferred.service, http.MethodPost, general.base+"/claims", r.redemption.Credential, claim)
			if scenario.ready {
				requireNativeStatus(t, response, http.StatusConflict)
				response = performHubAPIRequest(t, preferred.service, http.MethodPost, preferred.base+"/claims", r.redemption.Credential, providerClaim(r, preferredIssue, "preferred-provider"))
				generalIssue = preferredIssue
			}
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if lease.WorkItemID != generalIssue.WorkItemID || lease.ProviderReservation == nil {
				t.Fatalf("provider selection = %#v", lease)
			}
		})
	}
}

func TestRunnerAllowedWorkflowFilter(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	f.create(t, "earlier todo")
	rework := f.create(t, "eligible rework")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET detent_state = 'Rework' WHERE project_id = ? AND detent_state = 'In Progress'", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Rework') WHERE native_id = ?", f.project.ID, rework.WorkItemID); err != nil {
		t.Fatal(err)
	}
	claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID, SessionID: "rework-only", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: []string{" Rework "}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != rework.WorkItemID {
		t.Fatalf("rework claim = %#v", lease)
	}
}

func TestRunnerAllowedOrdering(t *testing.T) {
	for _, scenario := range []struct {
		name                          string
		firstPriority, secondPriority int
		firstRank, secondRank         int
		firstAge, secondAge           time.Duration
		allowed                       string
		wantFirst                     bool
	}{
		{"allowlist filters higher priority", 1, 4, 0, 1, -time.Hour, time.Hour, "second", false},
		{"empty allowlist runs nothing", 1, 4, 0, 1, -time.Hour, time.Hour, "empty", false},
		{"issue priority beats project rank", 2, 1, 0, 1, -time.Hour, time.Hour, "both", false},
		{"project rank breaks priority ties", 2, 2, 0, 1, time.Hour, -time.Hour, "both", true},
		{"age breaks remaining ties", 2, 2, 0, 0, time.Hour, -time.Hour, "both", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			first := newDefaultNativeFixture(t, Config{})
			second := newNativeFixture(t, first.service, first.project.OrganizationID, "second")
			r := prepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			allowed := []tracker.ProjectID{first.project.ID, second.project.ID}
			if scenario.allowed == "second" {
				allowed = []tracker.ProjectID{second.project.ID}
			}
			if scenario.allowed == "empty" {
				allowed = []tracker.ProjectID{}
			}
			routing := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: allowed}}
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing), http.StatusOK)
			descriptor := hubTestPolicy()
			now := first.service.config.now()
			for _, project := range []struct {
				fixture        nativeFixture
				priority, rank int
				age            time.Duration
			}{
				{first, scenario.firstPriority, scenario.firstRank, scenario.firstAge},
				{second, scenario.secondPriority, scenario.secondRank, scenario.secondAge},
			} {
				approveHubTestPolicy(t, first.service, project.fixture.base+"/policy", descriptor)
				issue := project.fixture.create(t, "ready")
				for _, statement := range []struct {
					sql  string
					args []any
				}{
					{"UPDATE projects SET scheduling_rank = ? WHERE id = ?", []any{project.rank, project.fixture.project.ID}},
					{"UPDATE queue_entries SET priority_override = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", []any{project.priority - 1, issue.WorkItemID}},
					{"UPDATE issues SET native_created_at = ? WHERE native_id = ?", []any{formatHubTime(now.Add(project.age)), issue.WorkItemID}},
				} {
					if _, err := first.service.database.db.ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "ordering", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			loser, winner := first, second
			if scenario.wantFirst {
				loser, winner = second, first
			}
			response := performHubAPIRequest(t, first.service, http.MethodPost, loser.base+"/claims", r.redemption.Credential, claim)
			if response.Code == http.StatusOK {
				t.Fatalf("lower-ranked or unauthorized project acquired work: %s", response.Body.String())
			}
			response = performHubAPIRequest(t, first.service, http.MethodPost, winner.base+"/claims", r.redemption.Credential, claim)
			if scenario.allowed == "empty" {
				if response.Code == http.StatusOK {
					t.Fatal("empty allowlist acquired work")
				}
			} else {
				requireNativeStatus(t, response, http.StatusOK)
			}
		})
	}
}
