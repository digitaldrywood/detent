package runnerfleet_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProviderStartRevalidation(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"exhausted", "model", "account", "active"} {
		t.Run(change, func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.Enroll(t)
			r.ReportCapacity(t)
			hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
			report := hubfixture.CapacityReport(f.Hub.Now())
			hubfixture.PublishCapacity(t, f, r, report)
			issue := f.Create(t, "work")
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, issue, "session"))
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			hubfixture.Decode(t, response, &lease)
			event := hubfixture.StartedEvent(lease)
			path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/events"
			if change == "active" {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, event), http.StatusOK)
			}
			switch change {
			case "exhausted", "active":
				report.Availability = "exhausted"
			case "model":
				event.Data.Identity.Model = "expensive"
			case "account":
				report.AccountAlias = "changed"
			}
			hubfixture.PublishCapacity(t, f, r, report)
			if change == "active" || change == "exhausted" {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
				event.Data.Handoff = hubfixture.Checkpoint()
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, event), http.StatusOK)
			} else {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, event), http.StatusConflict)
			}
		})
	}
}

func TestProviderOlderReportsCannotRestoreQuota(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.Enroll(t)
	report := hubfixture.CapacityReport(f.Hub.Now().Add(-time.Second))
	report.Availability = "exhausted"
	hubfixture.PublishCapacity(t, f, r, report)
	for _, age := range []time.Duration{0, -time.Second, time.Hour} {
		stale := report
		stale.ObservedAt = stale.ObservedAt.Add(age)
		stale.Availability = "available"
		hubfixture.PublishCapacity(t, f, r, stale)
		stored, err := []providercapacity.Report(nil), error(nil)
		err = f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			var readErr error
			stored, readErr = reader.RunnerProviderReports(t.Context(), r.Binding.RunnerID)
			return readErr
		})
		if err != nil || len(stored) != 1 || stored[0].Availability != "exhausted" || !stored[0].ObservedAt.Equal(report.ObservedAt) {
			t.Fatalf("stale observation replaced exhaustion: %+v, %v", stored, err)
		}
	}
}

func TestRunnerEnrollmentIdentityRevocationAndTenantScope(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read)
	r.Enroll(t)
	request := runnerauth.EnrollmentRequest{Binding: r.Binding, ProjectIDs: []tracker.ProjectID{f.Project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, r.IdentityPath(), hubfixture.AdminToken, nil), http.StatusNoContent)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, r.OrgBase+"/runner-enrollments", hubfixture.AdminToken, request), http.StatusConflict)
	response := f.Hub.Request(t, http.MethodPost, "/api/v2/organizations", hubfixture.AdminToken, map[string]string{"name": "other"})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var organization struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}
	hubfixture.Decode(t, response, &organization)
	other := hubfixture.NewNative(t, f.Hub, organization.ID, "foreign")
	request.Binding = runnerauth.NewBinding()
	request.ProjectIDs = []tracker.ProjectID{other.Project.ID}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, r.OrgBase+"/runner-enrollments", hubfixture.AdminToken, request), http.StatusUnprocessableEntity)
	live := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	live.Enroll(t)
	for _, path := range []string{other.Base, "/api/v2/organizations/" + string(organization.ID) + "/runners/" + live.Binding.RunnerID} {
		for _, test := range []struct {
			name, token string
			status      int
		}{
			{"valid credential outside scope", live.Redemption.Credential, http.StatusUnauthorized},
			{"revoked credential before resource lookup", r.Redemption.Credential, http.StatusUnauthorized},
			{"invalid credential before resource lookup", "invalid", http.StatusUnauthorized},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, path, test.token, nil), test.status)
			})
		}
	}
}

func TestSelfHostedRunnerVersionCompatibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		version      string
		major        int
		capabilities []string
		want         int
	}{
		{"older feature set", "0.117.56", 2, []string{"native_issues", "scoped_collaboration"}, http.StatusOK},
		{"current feature set", "current-release", 2, []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, http.StatusOK},
		{"unknown required feature", "future-release", 2, []string{"native_issues", "scoped_collaboration", "future_required"}, http.StatusUnprocessableEntity},
		{"unsupported major", "future-release", 99, []string{"native_issues", "scoped_collaboration"}, http.StatusUnprocessableEntity},
		{"missing required feature", "older-release", 2, []string{"native_issues"}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{Version: "0.118.0"})
			issue := f.Create(t, "work")
			hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.Redemption.Version = test.version
			r.Enroll(t)
			token := r.Redemption.Credential
			heartbeat := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", token, map[string]any{
				"version": test.version, "display_name": r.Redemption.DisplayName, "capacity": r.Redemption.Capacity, "backend_isolation": r.Redemption.BackendIsolation,
				"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: r.Redemption.Capacity, ClientLimit: r.Redemption.Capacity, RuntimeLimit: r.Redemption.Capacity, Manageable: true, ObservedAt: f.Hub.Now()},
			})
			hubfixture.RequireStatus(t, heartbeat, http.StatusOK)
			claim := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: issue.WorkItemID,
				MachineID: r.Binding.MachineID, SessionID: "version-session", TTLSeconds: 90,
				ProtocolMajor: test.major, Capabilities: test.capabilities}
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", token, claim)
			hubfixture.RequireStatus(t, response, test.want)
			if test.name == "older feature set" {
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				routing := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", token, nil)
				hubfixture.RequireStatus(t, routing, http.StatusOK)
				var runner runnerauth.Runner
				hubfixture.Decode(t, routing, &runner)
				if runner.Lifecycle != nil || runner.Health != "online" {
					t.Fatalf("legacy runner health=%q lifecycle=%+v", runner.Health, runner.Lifecycle)
				}
				path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/events"
				event := hubfixture.StartedEvent(lease)
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, token, event), http.StatusOK)
				event.Type, event.IdempotencyKey = "run.finished", "finish"
				event.Data.Sequence, event.Data.Outcome, event.Data.CompletionBody = 2, "succeeded", "Completed legacy runner attempt"
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, token, event), http.StatusOK)
			}
		})
	}
}
