package hubserver

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRoutingClaims(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		tags         []string
		state        string
		requirements policy.Requirements
		want         int
		lane         string
	}{
		{"empty selector", nil, "active", policy.Requirements{}, http.StatusOK, "Todo"},
		{"all tags", []string{"linux", "build"}, "active", policy.Requirements{RequiredTags: []string{"build", "linux"}}, http.StatusOK, "Todo"},
		{"missing tag", []string{"linux"}, "active", policy.Requirements{RequiredTags: []string{"build", "linux"}}, http.StatusConflict, "Todo"},
		{"unknown tag", []string{"linux"}, "active", policy.Requirements{RequiredTags: []string{"unknown"}}, http.StatusConflict, "Todo"},
		{"wrong machine", []string{"linux"}, "active", policy.Requirements{MachineID: string(runnerauth.NewBinding().MachineID)}, http.StatusConflict, "Todo"},
		{"wrong runner", []string{"linux"}, "active", policy.Requirements{RunnerID: runnerauth.NewBinding().RunnerID}, http.StatusConflict, "Todo"},
		{"draining", nil, "draining", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"disabled", nil, "disabled", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"draining rework", nil, "draining", policy.Requirements{}, http.StatusConflict, "Rework"},
		{"draining custom lane", nil, "draining", policy.Requirements{}, http.StatusConflict, "Repair Queue"},
		{"unreported configuration", nil, "active", policy.Requirements{}, http.StatusUnprocessableEntity, "Todo"},
		{"host full", nil, "active", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"draining broad claim", nil, "draining", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"candidate excluded", nil, "active", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"intake pending", nil, "active", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"isolation unsupported", nil, "active", policy.Requirements{}, http.StatusConflict, "Todo"},
		{"unreported configuration broad claim", nil, "active", policy.Requirements{}, http.StatusUnprocessableEntity, "Todo"},
		{"host full broad claim", nil, "active", policy.Requirements{}, http.StatusConflict, "Todo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "routing")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			issue := f.create(t, "queued")
			if test.lane != "Todo" {
				states := nativeFixtureStates()
				states[0].Name = test.lane
				for i := range states {
					for j, target := range states[i].Transitions {
						if target == "Todo" {
							states[i].Transitions[j] = test.lane
						}
					}
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					record.States = append(record.States, states[0])
					_, err = w.UpdateProjectRecord(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				issue = setSelectionFixtureState(t, f, issue.WorkItemID, test.lane)
			}
			settings := map[string]any{"expected_revision": 1, "display_name": "Build runner", "tags": test.tags, "state": test.state, "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, settings), http.StatusOK)
			r.reportCapacity(t)
			descriptor := hubTestPolicy()
			descriptor.Requirements = test.requirements
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			switch test.name {
			case "unreported configuration", "unreported configuration broad claim":
				claim.PolicyID = "stale"
			case "host full", "host full broad claim":
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE id=$1", r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			case "draining broad claim":
				claim.WorkItemID = ""
			case "candidate excluded":
				claim.LabelInclude = []string{"missing"}
			case "isolation unsupported":
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerBackendIsolation(t.Context(), r.binding.RunnerID, isolation.Report{})
				}); err != nil {
					t.Fatal(err)
				}
				r.redemption.BackendIsolation = nil
			case "intake pending":
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					imports := w.(projectstore.GitHubImportWriter)
					record, err := imports.CreateGitHubImport(t.Context(), projectstore.GitHubImport{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: hubapi.NewID("import"), IssueNumber: 1, ObservedAt: f.service.config.Clock()})
					if err != nil {
						return err
					}
					record.WorkItemID, record.IntakePending = string(issue.WorkItemID), true
					_, err = imports.UpdateGitHubImport(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "unreported configuration broad claim" || test.name == "host full broad claim" {
				claim.WorkItemID = ""
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
			var leases int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil {
				t.Fatal(err)
			}
			wantLeases := 0
			if test.want == http.StatusOK {
				wantLeases = 1
			}
			if leases != wantLeases {
				t.Fatalf("leases = %d, want %d", leases, wantLeases)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "Build runner", "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation})
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			decodeHubResponse(t, response, &snapshot)
			if snapshot.ClaimState == nil || snapshot.ClaimState.PolicyIDs[f.project.ID] != descriptor.ID || len(snapshot.ClaimState.Slots) != wantLeases || snapshot.ClaimState.RunnerCapacity != 2 {
				t.Fatalf("heartbeat claim facts = %#v", snapshot.ClaimState)
			}
			if wantLeases > 0 && snapshot.ClaimState.Slots[0].RunnerID != r.binding.RunnerID {
				t.Fatalf("heartbeat slot owner = %#v", snapshot.ClaimState.Slots)
			}
			if test.name == "host full" || test.name == "host full broad claim" {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE id=$1", r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			runtimePath := f.base + "/work-items/" + string(issue.WorkItemID) + "/runtime"
			var evidence tracker.NativeRuntimeEvidence
			readDecision := func() {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &evidence)
			}
			readDecision()
			if test.state == "disabled" || test.want == http.StatusUnprocessableEntity {
				if evidence.LatestDecision != nil {
					t.Fatal("pre-evaluation refusal manufactured a scheduling decision")
				}
				return
			}
			if evidence.LatestDecision == nil || evidence.LatestDecision.Actor.Kind != "runner" || evidence.LatestDecision.Data.Decision.RunnerID != r.binding.RunnerID || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision || evidence.LatestDecision.Data.Change != nil || evidence.Issue.State != test.lane {
				t.Fatalf("claim evaluation lost scoped evidence: %#v", evidence)
			}
			decision := evidence.LatestDecision
			decisionID := decision.ID
			if test.want == http.StatusOK {
				if decision.Data.Decision.Outcome != "claimed" || decision.Data.Decision.Source != "native_claim" {
					t.Fatalf("claim evidence = %#v", decision)
				}
				var rows []eventstore.Event
				err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					rows, err = reader.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
					return err
				})
				if len(rows) == 100 {
					t.Fatal("decision fixture history exceeds complete read bound")
				}
				evaluatedLanding := 0
				for _, row := range rows {
					if err != nil || row.Type != "scheduler.decision" {
						continue
					}
					var event tracker.CollaborationEvent
					event, err = eventstore.NewContent(f.service.runDetailStoreFor(t.Context())).Event(t.Context(), row)
					if err == nil && event.Data.Decision != nil && event.Data.Decision.Source == "native_claim_eligibility" {
						evaluatedLanding++
					}
				}
				if err != nil || evaluatedLanding != 0 {
					t.Fatalf("ordinary claim manufactured Change readiness: count=%d error=%v", evaluatedLanding, err)
				}
				if test.name == "empty selector" {
					foreign := newNativeFixture(t, f.service, "", "foreign-routing")
					claim.WorkItemID = foreign.create(t, "foreign").WorkItemID
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/claims", r.redemption.Credential, claim), http.StatusNotFound)
					var count int
					if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE project_id=$1 AND type='scheduler.decision'", foreign.project.ID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("foreign refusal mutated history: count=%d error=%v", count, err)
					}
				}
				return
			}
			wantReason := "selector_no_match"
			switch {
			case test.state == "draining":
				wantReason = "runner_draining"
			case test.name == "host full" || test.name == "host full broad claim":
				wantReason = "host_capacity"
			case test.name == "candidate excluded" || test.name == "intake pending" || test.name == "isolation unsupported":
				wantReason = "no_claimable_work"
			}
			if decision.Data.Decision.Outcome != "skipped" || decision.Data.Decision.Reason != wantReason || evidence.NativeCandidateExclusion == nil || evidence.NativeCandidateExclusion.ID != decision.ID {
				t.Fatalf("refusal evidence = %#v", evidence)
			}
			explanation := explain.FromNativeEvidence(evidence)
			if !slices.ContainsFunc(explanation.Sources, func(source explain.SourceStatus) bool {
				return source.Name == "native_candidate_exclusion" && source.State == explain.SourceAvailable
			}) {
				t.Fatal("explain_item omitted recorded refusal availability")
			}
			for _, admission := range explanation.NativeRuntime.Admission {
				if slices.Contains(admission.Unavailable, "native_candidate_exclusion") {
					t.Fatal("explain_item marked the recorded exclusion unavailable")
				}
			}
			if test.name == "intake pending" && !slices.Contains(evidence.Admission[0].Unavailable, "other_native_candidate_exclusions") {
				t.Fatal("historical refusal hid unavailable current exclusion detail")
			}
			for range 3 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
			}
			readDecision()
			if evidence.LatestDecision.ID != decisionID {
				t.Fatal("unchanged refused evaluation appended duplicate history")
			}
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=$1 AND type='scheduler.decision'", issue.WorkItemID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("repeated polls recorded %d decisions: %v", count, err)
			}
			now := time.Now().UTC()
			scope := analyticsTestScope(t, f)
			analytics, err := f.service.readNativeAnalyticsTenant(t.Context(), scope, operatortool.AnalyticsRequest{}, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour})
			if err != nil || len(analytics.SkipReasons) != 1 || analytics.SkipReasons[0].Reason != wantReason || analytics.SkipReasons[0].Count != 1 {
				t.Fatalf("skip_reasons = %#v: %v", analytics.SkipReasons, err)
			}
			if test.name == "draining" {
				claim.PolicyID = "stale"
				for range 2 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusUnprocessableEntity)
				}
				claim.PolicyID = descriptor.ID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=$1 AND type='scheduler.decision'", issue.WorkItemID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("invalid request changed recorded %d decisions: %v", count, err)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "advance-revision"}, ExpectedRevision: issue.Revision, Title: new("Updated issue")}), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
			readDecision()
			if evidence.LatestDecision.ID == decisionID || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision+1 {
				t.Fatalf("changed revision lost fresh refusal: event=%s original=%s decision=%+v expected=%d", evidence.LatestDecision.ID, decisionID, *evidence.LatestDecision.Data.Decision, issue.Revision+1)
			}
			if test.name == "draining" {
				refusalID := evidence.NativeCandidateExclusion.ID
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET state='active' WHERE runner_id=$1", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				claim.SessionID = "accepted"
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				readDecision()
				if evidence.LatestDecision.Data.Decision.Outcome != "claimed" || evidence.NativeCandidateExclusion.ID != refusalID {
					t.Fatal("successful claim lost the historical refusal")
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET state='draining' WHERE runner_id=$1", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				claim.SessionID = "next-cycle"
				for range 2 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				}
				readDecision()
				if evidence.NativeCandidateExclusion.ID == refusalID {
					t.Fatal("successful claim did not start a new evaluation cycle")
				}
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=$1 AND type='scheduler.decision'", issue.WorkItemID).Scan(&count); err != nil || count != 4 {
					t.Fatalf("claim cycle recorded %d decisions: %v", count, err)
				}
				other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				other.enroll(t)
				other.reportCapacity(t)
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET state='draining' WHERE runner_id=$1", other.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				claim.MachineID, claim.SessionID = other.binding.MachineID, "other-runner"
				for range 2 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", other.redemption.Credential, claim), http.StatusConflict)
				}
				readDecision()
				if evidence.NativeCandidateExclusion.Data.Decision.RunnerID != other.binding.RunnerID {
					t.Fatal("another runner's refusal was deduplicated away")
				}
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=$1 AND type='scheduler.decision'", issue.WorkItemID).Scan(&count); err != nil || count != 5 {
					t.Fatalf("runner refusals recorded %d decisions: %v", count, err)
				}
			}
		})
	}
}

func TestRunnerSettingsPersistAndReachHeartbeat(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }}), "", "settings")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{
		DisplayName: "Build runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID},
		IsolationTier: "native-trusted", HostServices: []string{"tcp:127.0.0.1:8080"},
		Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": 1}, HardDeadline: "30m"},
	}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
	var stored runnerauth.Runner
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &stored)
	if stored.Revision != 2 || stored.Availability.WindowSlots["Mon-Fri 09:00-17:00"] != 1 || stored.IsolationTier != "native-trusted" || stored.Availability.HardDeadline != "30m" || len(stored.HostServices) != 1 {
		t.Fatalf("stored settings = %#v", stored.Routing)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusConflict)
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/projects/"+string(f.project.ID)+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"display_name": "Build runner", "capacity": 2, "version": "test", "os": "linux", "architecture": "arm64"})
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &snapshot)
	if !maps.Equal(snapshot.Routing.Availability.WindowSlots, change.Availability.WindowSlots) || snapshot.RunnerID != r.binding.RunnerID || snapshot.Revision != stored.Revision || snapshot.Routing.IsolationTier != "native-trusted" || snapshot.Routing.Availability.HardDeadline != "30m" {
		t.Fatalf("heartbeat routing = %#v", snapshot)
	}
	legacy := runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: runnerauth.Routing{DisplayName: "Renamed", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, legacy)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if !maps.Equal(stored.Availability.WindowSlots, change.Availability.WindowSlots) || stored.DisplayName != "Renamed" || stored.IsolationTier != "native-trusted" {
		t.Fatalf("legacy routing update lost settings: %#v", stored.Routing)
	}
	partial := map[string]any{"expected_revision": stored.Revision, "display_name": "Renamed", "tags": stored.Tags, "state": stored.State,
		"capacity_limit": stored.CapacityLimit, "project_ids": stored.ProjectIDs, "isolation_tier": "sandbox"}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, partial)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if stored.IsolationTier != "sandbox" || len(stored.HostServices) != 1 || stored.Availability.HardDeadline != "30m" {
		t.Fatalf("partial routing update lost settings: %#v", stored.Routing)
	}
	partial["expected_revision"] = stored.Revision
	partial["host_services"] = []string{}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, partial)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if len(stored.HostServices) != 0 || stored.Availability.HardDeadline != "30m" {
		t.Fatalf("explicit host service clear lost settings: %#v", stored.Routing)
	}
	partial["expected_revision"] = stored.Revision
	partial["availability"] = runnerauth.Availability{Windows: []string{}}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, partial)
	requireNativeStatus(t, response, http.StatusOK)
	stored = runnerauth.Runner{}
	decodeHubResponse(t, response, &stored)
	if len(stored.Availability.WindowSlots) != 0 || stored.IsolationTier != "sandbox" || len(stored.Availability.Windows) != 0 || stored.Availability.HardDeadline != "" {
		t.Fatalf("explicit availability clear lost settings: %#v", stored.Routing)
	}
	disabled := runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: stored.Routing}
	disabled.State = "disabled"
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, disabled)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	heartbeat := map[string]any{"display_name": "Build runner", "capacity": 2, "version": "test", "os": "linux", "architecture": "arm64"}
	response = performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/projects/"+string(f.project.ID)+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &snapshot)
	if snapshot.Routing.State != "disabled" || snapshot.Revision != stored.Revision {
		t.Fatalf("disabled runner heartbeat routing = %#v", snapshot)
	}
	unassigned := runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: stored.Routing}
	unassigned.State = "active"
	unassigned.ProjectIDs = []tracker.ProjectID{}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, unassigned)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	response = performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/projects/"+string(f.project.ID)+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &snapshot)
	if len(snapshot.Routing.ProjectIDs) != 0 || snapshot.Revision != stored.Revision {
		t.Fatalf("unassigned runner heartbeat routing = %#v", snapshot)
	}

	for _, test := range []struct {
		name    string
		elapsed time.Duration
		expired bool
		revoked bool
		want    string
	}{
		{name: "heartbeat committed while read waits", elapsed: 1338 * time.Millisecond, want: "online"},
		{name: "stale heartbeat", elapsed: runnerauth.HeartbeatTimeout, want: "offline"},
		{name: "future heartbeat", elapsed: -time.Nanosecond, want: "offline"},
		{name: "expired credential", expired: true, want: "expired"},
		{name: "revoked credential", revoked: true, want: "revoked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := f.service.config.Clock().UTC().Truncate(time.Microsecond)
			heartbeatAt := before.Add(time.Second)
			observedAt := heartbeatAt.Add(test.elapsed)
			clock.Store(before.UnixNano())
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if err != nil {
				t.Fatalf("timing reader status=%d error=%v", status, err)
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseRead := sync.OnceFunc(func() { close(release) })
			ctx := postgres.WithTransactionTiming(t.Context(), func(name string, _ time.Time) {
				if name == "pg_admission" {
					close(entered)
					<-release
				}
			})
			var result runnerauth.Runner
			var readErr error
			go func() {
				defer close(done)
				result, readErr = f.service.readRunnerRouting(ctx, hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: credential}, r.binding.RunnerID)
			}()
			defer func() { releaseRead(); <-done }()
			select {
			case <-entered:
			case <-done:
				t.Fatalf("routing read returned before admission barrier: %v", readErr)
			}
			tx, err := usagePostgresAdmin(t, f.service).BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2 AND runner_id=$3", heartbeatAt, f.project.OrganizationID, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			expiresAt := r.identity.ExpiresAt
			if test.expired {
				expiresAt = observedAt
			}
			var revokedAt any
			if test.revoked {
				revokedAt = heartbeatAt
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE api_tokens SET expires_at=$1,revoked_at=$2 WHERE organization_id=$3 AND id=(SELECT token_id FROM runner_identities WHERE organization_id=$3 AND runner_id=$4)", expiresAt, revokedAt, f.project.OrganizationID, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			clock.Store(observedAt.UnixNano())
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			releaseRead()
			<-done
			if readErr != nil {
				t.Fatal(readErr)
			}
			if result.Health != test.want || result.ConnectionHealth != test.want || !result.LastHeartbeatAt.Equal(heartbeatAt) {
				t.Fatalf("runner observed at %s: health=%s connection_health=%s heartbeat=%s, want %s at %s", observedAt, result.Health, result.ConnectionHealth, result.LastHeartbeatAt, test.want, heartbeatAt)
			}
		})
	}
}

func sharedRunner(t *testing.T, first runnerFixture) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	binding.MachineID = first.binding.MachineID
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	r := runnerFixture{nativeFixture: first.nativeFixture, binding: binding, base: first.base,
		redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "shared", DisplayName: "Second", Capacity: 8, Version: "test", OS: "linux", Architecture: "arm64"}}
	response := performHubAPIRequest(t, r.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken,
		runnerauth.EnrollmentRequest{Binding: binding, SharedMachine: true, ProjectIDs: []tracker.ProjectID{r.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &r.enrollment)
	r.enroll(t)
	return r
}

func assertRunnerAdmissionFacts(t *testing.T, f nativeFixture, r runnerFixture, joining ...string) runnerauth.Runner {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		t.Fatalf("admission reader status=%d error=%v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	full, err := f.service.readRunnerRouting(t.Context(), scope, r.binding.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := f.service.config.Clock()
	var snapshot runnerstore.RuntimeCapacitySnapshot
	var identity runnerstore.Identity
	var backend isolation.Report
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		snapshot, err = reader.(runnerstore.RuntimeCapacityReader).RuntimeCapacitySnapshot(t.Context(), runnerstore.RuntimeCapacityQuery{ProjectID: scope.Project, RunnerID: r.binding.RunnerID, At: now})
		if err != nil {
			return err
		}
		identity, err = reader.RunnerIdentity(t.Context(), r.binding.RunnerID)
		if err != nil {
			return err
		}
		backend, err = reader.RunnerBackendIsolation(t.Context(), r.binding.RunnerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runners := runtimeRunnersFromSnapshot(snapshot, scope, now)
	if full.Scope != "organization" && !slices.Contains(full.ProjectIDs, scope.Project) {
		if len(runners) != 0 {
			t.Fatalf("admission snapshot exposed a runner without project access: %+v", runners)
		}
		return full
	}
	if len(runners) != 1 || runners[0].RunnerID != r.binding.RunnerID {
		t.Fatalf("admission snapshot returned unexpected runners: %+v", runners)
	}
	admission := runners[0]
	if len(admission.Leases) != 0 {
		t.Fatal("dispatch read includes display lease details")
	}
	wantBackend := make(map[string][]string, len(backend))
	for name, tiers := range backend {
		for _, tier := range tiers {
			wantBackend[name] = append(wantBackend[name], string(tier))
		}
	}
	wantAvailability := runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: append([]string{}, identity.Availability.Windows...), WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline}
	if !reflect.DeepEqual(full.BackendIsolation, wantBackend) || !reflect.DeepEqual(full.CapacityConfig, identity.CapacityConfig) || !reflect.DeepEqual(full.Availability, wantAvailability) || full.IsolationTier != identity.IsolationTier || full.MachineUse != identity.MachineUse || !slices.Equal(full.HostServices, identity.HostServices) || !maps.Equal(full.ProjectCapacityLimits, identity.ProjectCapacityLimits) || !maps.Equal(full.ProjectRankOverrides, identity.ProjectRanks) {
		t.Fatalf("routing dispatch configuration differs from typed authority: routing=%+v identity=%+v backend=%+v", full, identity, backend)
	}
	comparable := full
	comparable.Leases, comparable.Problems = nil, nil
	comparable.BackendIsolation, comparable.CapacityConfig, comparable.Update = nil, nil, nil
	comparable.HostDisplayName, comparable.HostRevision, comparable.OS, comparable.Architecture = "", 0, "", ""
	comparable.DisplayName, comparable.MachineUse, comparable.IsolationTier = "", "", ""
	comparable.HostServices, comparable.Availability = nil, runnerauth.Availability{}
	comparable.UpdateRequest, comparable.ProjectConfigurationCommand, comparable.CapacityRequest = nil, nil, nil
	comparable.ProjectRanks, comparable.ProjectRankOverrides = nil, nil
	if len(comparable.ProviderCapacity) == 0 {
		comparable.ProviderCapacity = nil
	}
	if len(admission.ProviderCapacity) == 0 {
		admission.ProviderCapacity = nil
	}
	if !reflect.DeepEqual(comparable, admission) {
		t.Fatalf("admission facts differ from full runner read: full=%+v admission=%+v", comparable, admission)
	}
	if len(joining) > 0 && joining[0] != "" {
		batch, found := strings.CutPrefix(joining[0], string(r.binding.MachineID)+"/"+string(scope.Project)+"/")
		if !found || batch == "" {
			t.Fatalf("invalid joining group %s", joining[0])
		}
		var usage claimstore.CapacityUsage
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			usage, err = reader.ClaimCapacityUsage(t.Context(), string(r.binding.MachineID), r.binding.RunnerID, scope.Project, batch, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		remaining := snapshot
		remaining.Leases = nil
		for _, lease := range snapshot.Leases {
			if lease.RunnerID == r.binding.RunnerID && landingCapacityKey(lease.MachineID, lease.CapacityProject, lease.SessionID) == joining[0] {
				continue
			}
			remaining.Leases = append(remaining.Leases, lease)
		}
		joined := runtimeRunnersFromSnapshot(remaining, scope, now)[0]
		if int64(joined.HostUsed) != usage.Host || int64(joined.Used) != usage.Runner || int64(joined.ProjectUsed[scope.Project]) != usage.Project {
			t.Fatalf("joining capacity differs from retained lease snapshot: expected=%+v usage=%+v", joined, usage)
		}
		admission.HostUsed, admission.Used = int(usage.Host), int(usage.Runner)
		if admission.ProjectUsed == nil {
			admission.ProjectUsed = map[tracker.ProjectID]int{}
		}
		admission.ProjectUsed[scope.Project] = int(usage.Project)
	}
	return admission
}

func TestRunnerSharedHostConcurrentClaimsAndRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cfg := Config{Clock: func() time.Time { return now }}
	f := newNativeFixture(t, newClockedPostgresNativeService(t, cfg), "", "shared")
	otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "other-shared")
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	first.enroll(t)
	second := sharedRunner(t, first)
	for _, r := range []runnerFixture{first, second} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "Shared runner", "state": "active", "capacity_limit": 8, "project_ids": []tracker.ProjectID{f.project.ID, otherProject.project.ID}}), http.StatusOK)
		r.reportCapacity(t)
	}
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	approveHubTestPolicy(t, f.service, otherProject.base+"/policy", descriptor)
	start := make(chan struct{})
	type outcome struct {
		runner runnerFixture
		claim  tracker.NativeClaim
		lease  tracker.NativeLease
		status int
	}
	results := make(chan outcome, 8)
	var workers sync.WaitGroup
	for i := range 8 {
		r := first
		project := f
		if i%2 != 0 {
			r = second
			project = otherProject
		}
		r.nativeFixture = project
		r.base = project.base
		issue := project.create(t, fmt.Sprintf("work-%d", i))
		claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: fmt.Sprintf("session-%d", i), TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
		workers.Go(func() {
			<-start
			response := performHubAPIRequest(t, f.service, http.MethodPost, project.base+"/claims", r.redemption.Credential, claim)
			result := outcome{runner: r, claim: claim, status: response.Code}
			if response.Code == http.StatusOK {
				decodeHubResponse(t, response, &result.lease)
			}
			results <- result
		})
	}
	close(start)
	workers.Wait()
	close(results)
	winners := []outcome{}
	for result := range results {
		if result.status == http.StatusOK {
			winners = append(winners, result)
		} else if result.status != http.StatusConflict {
			t.Fatalf("claim status = %d", result.status)
		}
	}
	if len(winners) != 2 {
		t.Fatalf("shared host allocated %d leases, want 2", len(winners))
	}
	for _, runner := range []runnerFixture{first, second} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": "Shared runner", "capacity": 8, "version": "test"})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		if snapshot.ClaimState == nil || snapshot.ClaimState.HostCapacity != 2 || len(snapshot.ClaimState.Slots) != 2 {
			t.Fatalf("shared host heartbeat facts = %#v", snapshot.ClaimState)
		}
	}
	for _, winner := range winners {
		other := first
		if winner.runner.binding.RunnerID == first.binding.RunnerID {
			other = second
		}
		path := winner.runner.base + "/leases/" + string(winner.lease.ID)
		for _, operation := range []string{"renew", "release", "validate"} {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+operation, other.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken, TTLSeconds: 300, Reason: "released"}), http.StatusNotFound)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, winner.runner.base+"/claims", winner.runner.redemption.Credential, winner.claim), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/validate", winner.runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken}), http.StatusOK)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	response := performHubAPIRequest(t, f.service, http.MethodGet, first.base+"/runners", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var fleet []runnerauth.Runner
	decodeHubResponse(t, response, &fleet)
	if len(fleet) != 2 {
		t.Fatalf("fleet = %#v", fleet)
	}
	for _, r := range fleet {
		fixture := first
		fixture.binding.RunnerID = r.RunnerID
		admission := assertRunnerAdmissionFacts(t, f, fixture)
		for _, project := range []tracker.ProjectID{f.project.ID, otherProject.project.ID} {
			if !reflect.DeepEqual(r.Exclusions(project, descriptor.Requirements, false), admission.Exclusions(project, descriptor.Requirements, false)) {
				t.Fatal("admission read changed shared-host claim eligibility")
			}
		}
		if r.HostUsed != 2 || r.HostCapacity != 2 {
			t.Fatalf("host capacity after restart = %#v", r)
		}
		if len(r.Leases) != r.Used {
			t.Fatalf("active run detail count = %d, used = %d", len(r.Leases), r.Used)
		}
		for _, lease := range r.Leases {
			if lease.Policy.ID != descriptor.ID || len(lease.Exclusions) != 0 || lease.Title == "" {
				t.Fatalf("run eligibility lost pinned policy: %#v", lease)
			}
		}
	}
	now = now.Add(301 * time.Second)
	winner := winners[0]
	winner.claim.SessionID = "offline-target"
	response = performHubAPIRequest(t, f.service, http.MethodPost, winner.runner.base+"/claims", winner.runner.redemption.Credential, winner.claim)
	requireNativeStatus(t, response, http.StatusConflict)
	var failure hubapi.NativeError
	decodeHubResponse(t, response, &failure)
	if failure.Code != "runner_offline" {
		t.Fatalf("offline reason = %s", failure.Code)
	}
}

func TestRunnerRoutingRevocationAndDrain(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"projects", "organization"} {
		for _, test := range []struct {
			name, state string
			tags        []string
			access      bool
			want        int
		}{
			{"rename", "active", []string{"build"}, true, http.StatusOK},
			{"drain active lease", "draining", []string{"build"}, true, http.StatusOK},
			{"disable", "disabled", []string{"build"}, true, http.StatusConflict},
			{"remove required tag", "active", nil, true, http.StatusConflict},
			{"remove access", "active", []string{"build"}, false, http.StatusNotFound},
		} {
			t.Run(scope+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				f := newNativeFixture(t, nil, "", "revocation")
				r := prepareRunnerForScope(t, f, scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				r.enroll(t)
				change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{Scope: scope, DisplayName: "First", State: "active", Tags: []string{"build"}, CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
				if scope == "organization" {
					change.ProjectIDs = nil
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
				r.reportCapacity(t)
				descriptor := hubTestPolicy()
				descriptor.Requirements.RequiredTags = []string{"build"}
				descriptor = descriptor.WithID()
				approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
				issue := f.create(t, "work")
				claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "work", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				change.ExpectedRevision, change.DisplayName, change.State, change.Tags = 2, "Renamed", test.state, test.tags
				if !test.access {
					change.Scope = "projects"
					change.ProjectIDs = nil
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
				for _, action := range []string{"renew", "validate"} {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/"+action, r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), test.want)
				}
				event := nativeStartedEvent(lease)
				event.IdempotencyKey, event.Data.PolicyID = "event", descriptor.ID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), test.want)
			})
		}
	}
}

func TestRunnerLeaseValidationAccounting(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"projects", "organization"} {
		for _, test := range []struct {
			name, statement, target, code string
			apiLimit                      int64
			fenceOffset                   tracker.FencingToken
			status                        int
		}{
			{name: "available with exhausted event window", apiLimit: 100, status: http.StatusOK},
			{name: "API window exhausted", apiLimit: 0, status: http.StatusTooManyRequests, code: "allowance_exhausted"},
			{name: "stale fence", apiLimit: 100, fenceOffset: 1, status: http.StatusConflict, code: "stale_fencing_token"},
			{name: "expired lease", apiLimit: 100, statement: "UPDATE leases SET acquired_at=acquired_at-interval '2 minutes',renewed_at=acquired_at-interval '2 minutes',expires_at=acquired_at-interval '1 minute' WHERE lease_id=$1", target: "lease", status: http.StatusConflict, code: "stale_fencing_token"},
			{name: "released lease", apiLimit: 100, statement: "UPDATE leases SET released_at=acquired_at WHERE lease_id=$1", target: "lease", status: http.StatusConflict, code: "stale_fencing_token"},
			{name: "revoked credential", apiLimit: 100, statement: "UPDATE api_tokens SET revoked_at=created_at WHERE id=$1", target: "runner", status: http.StatusUnauthorized, code: "unauthorized"},
			{name: "project access removed", apiLimit: 100, statement: "DELETE FROM token_grants WHERE token_id=$1", target: "runner", status: http.StatusNotFound, code: "not_found"},
			{name: "disabled runner", apiLimit: 100, statement: "UPDATE runner_identities SET state='disabled' WHERE runner_id=$1", target: "runner", status: http.StatusConflict, code: "runner_disabled"},
			{name: "wrong runner", apiLimit: 100, status: http.StatusNotFound, code: "not_found"},
			{name: "cross organization", apiLimit: 100, status: http.StatusNotFound, code: "not_found"},
			{name: "removed approval preserves lease", apiLimit: 100, statement: "DELETE FROM project_policies WHERE organization_id||'/'||project_id=$1", target: "project", status: http.StatusOK},
		} {
			t.Run(scope+"/"+test.name, func(t *testing.T) {
				if scope == "organization" && test.name == "project access removed" {
					test.status, test.code = http.StatusOK, ""
				}
				now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
				f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }}), "", "lease-accounting")
				r := prepareRunnerForScope(t, f, scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.enroll(t)
				r.reportCapacity(t)
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				issue := f.create(t, "work")
				claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "work", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				d := f.service.database
				seedHubMachine(t, f.service, f.project.OrganizationID, "unrelated")
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET last_heartbeat_at=$1 WHERE organization_id=$2 AND id='unrelated'", now.Add(-48*time.Hour), f.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
				if test.statement != "" {
					var target any = r.binding.RunnerID
					switch test.target {
					case "lease":
						target = lease.ID
					case "project":
						target = string(f.project.OrganizationID) + "/" + string(f.project.ID)
					}
					if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), test.statement, target); err != nil {
						t.Fatal(err)
					}
				}
				credential, base := r.redemption.Credential, f.base
				switch test.name {
				case "wrong runner":
					other := prepareRunnerForScope(t, f, scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					other.enroll(t)
					credential = other.redemption.Credential
				case "cross organization":
					organization := tracker.OrganizationID(hubapi.NewID("org"))
					seedPostgresHubAdmin(t, f.service.config.TenantRepository, f.service, nativeOrganization{ID: organization, Name: "Other organization", Slug: string(organization)})
					other := newNativeFixture(t, f.service, organization, "other-project")
					if other.project.OrganizationID == f.project.OrganizationID || other.project.ID == f.project.ID {
						t.Fatal("cross-organization fixtures must have distinct organizations and projects")
					}
					otherRunner := prepareRunnerForScope(t, other, scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					otherRunner.enroll(t)
					otherRunner.reportCapacity(t)
					approveHubTestPolicy(t, f.service, other.base+"/policy", hubTestPolicy())
					otherIssue := other.create(t, "other work")
					otherClaim := claim
					otherClaim.SessionID = "other-work"
					otherClaim.WorkItemID, otherClaim.MachineID = otherIssue.WorkItemID, otherRunner.binding.MachineID
					response := performHubAPIRequest(t, f.service, http.MethodPost, other.base+"/claims", otherRunner.redemption.Credential, otherClaim)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &lease)
					base = other.base
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/validate", otherRunner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}), http.StatusOK)
					otherClaim.MachineID = r.binding.MachineID
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/claims", credential, otherClaim), http.StatusNotFound)
				}
				f.service.config.Hosted = &hostedplans.HostedConfig{}
				d.hostedOrganization = f.project.OrganizationID
				hostedTestPlans(t, f.service, map[string]int64{"api_mutations": test.apiLimit, "ingested_events": 0, "collaboration_bytes": 0, "history_records": 0})
				f.service.config.Hosted = nil
				metrics := hostedRunnerTransactionMetrics(hubapi.NativeBase + "/leases/:lease/validate")
				before, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, now, metrics...))
				if err != nil {
					t.Fatal(err)
				}
				response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/validate", credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken + test.fenceOffset})
				requireNativeStatus(t, response, test.status)
				if test.code != "" {
					var failure hubapi.APIErrorResponse
					decodeHubResponse(t, response, &failure)
					if failure.Code != test.code {
						t.Fatalf("refusal=%s want=%s", failure.Code, test.code)
					}
				}
				after, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, now, metrics...))
				if err != nil {
					t.Fatal(err)
				}
				if test.status == http.StatusOK {
					var adopted runnerauth.Runner
					decodeHubResponse(t, response, &adopted)
					if adopted.RunnerID != r.binding.RunnerID || adopted.OrganizationID != f.project.OrganizationID {
						t.Fatalf("validated runner = %#v", adopted.Binding)
					}
					before["api_mutations"]++
				}
				if !maps.Equal(before, after) {
					t.Fatalf("validation accounting=%v want=%v", after, before)
				}
				var expires time.Time
				if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT expires_at FROM leases WHERE lease_id=$1", lease.ID).Scan(&expires); err != nil {
					t.Fatal(err)
				}
				if test.name != "expired lease" && !expires.Equal(lease.ExpiresAt) {
					t.Fatal("validation changed lease expiry")
				}
			})
		}
	}
}

func TestRunnerRoutingAdministratorControls(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*runnerauth.RoutingChange)
		worker bool
		want   int
	}{
		{"normalized tags", func(c *runnerauth.RoutingChange) { c.Tags = []string{" LINUX ", "Build", "linux"} }, false, http.StatusOK},
		{"worker self assignment", func(*runnerauth.RoutingChange) {}, true, http.StatusForbidden},
		{"stale revision", func(c *runnerauth.RoutingChange) { c.ExpectedRevision = 5 }, false, http.StatusConflict},
		{"invalid tag", func(c *runnerauth.RoutingChange) { c.Tags = []string{"not a tag"} }, false, http.StatusUnprocessableEntity},
		{"invalid state", func(c *runnerauth.RoutingChange) { c.State = "unknown" }, false, http.StatusUnprocessableEntity},
		{"negative capacity", func(c *runnerauth.RoutingChange) { c.CapacityLimit = -1 }, false, http.StatusUnprocessableEntity},
		{"nonowner allowlist edit", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{} }, false, http.StatusForbidden},
		{"unknown project", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{"prj_unknown"} }, false, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "admin")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			if test.name == "nonowner allowlist edit" {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_enrollments SET created_by = (SELECT id FROM api_tokens WHERE token_hash = $1) WHERE runner_id = $2", apikey.HashToken(f.token), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Trusted", State: "active", Tags: []string{"production"}, CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
			test.change(&change)
			token := testHubAdminToken
			if test.worker {
				token = r.redemption.Credential
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", token, change), test.want)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, r.base+"/runners", r.redemption.Credential, nil), http.StatusForbidden)
			response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var before runnerauth.Runner
			decodeHubResponse(t, response, &before)
			if test.want == http.StatusOK && (len(before.Tags) != 2 || before.Tags[0] != "build" || before.Tags[1] != "linux") {
				t.Fatalf("tags = %#v", before.Tags)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", r.redemption.Credential, map[string]any{"id": r.binding.MachineID, "hostname": "forged", "display_name": "forged", "capacity": 999, "version": "test", "os": "linux", "architecture": "arm64"}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil)
			var after runnerauth.Runner
			decodeHubResponse(t, response, &after)
			if after.HostCapacity != 2 || after.DisplayName != before.DisplayName || after.CapacityLimit != before.CapacityLimit || after.Hostname != before.Hostname || after.OS != "linux" {
				t.Fatalf("worker changed administrator controls: %#v", after)
			}
			if test.want != http.StatusOK && len(after.Tags) != 0 {
				t.Fatalf("rejected edit assigned tags: %#v", after.Tags)
			}
		})
	}
}

func TestRepositoryRunnerSelectorsCannotStealWork(t *testing.T) {
	t.Parallel()
	a := newNativeFixture(t, nil, "", "mac-project")
	b := newNativeFixture(t, a.service, a.project.OrganizationID, "linux-project")
	mac := prepareRunner(t, a, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	mac.enroll(t)
	linux := prepareRunner(t, b, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	linux.enroll(t)
	for _, r := range []runnerFixture{mac, linux} {
		tags := []string{"macos"}
		if r.binding.RunnerID == linux.binding.RunnerID {
			tags = []string{"linux", "build"}
		}
		change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Builder", State: "active", Tags: tags, CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{a.project.ID, b.project.ID}}}
		requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
		r.reportCapacity(t)
	}
	macPolicy := hubTestPolicy()
	macPolicy.Requirements.MachineID = string(mac.binding.MachineID)
	macPolicy = macPolicy.WithID()
	linuxPolicy := hubTestPolicy()
	linuxPolicy.Requirements.RequiredTags = []string{"build", "linux"}
	linuxPolicy = linuxPolicy.WithID()
	approveHubTestPolicy(t, a.service, a.base+"/policy", macPolicy)
	approveHubTestPolicy(t, a.service, b.base+"/policy", linuxPolicy)
	aIssue := a.create(t, "exact Mac work")
	bIssue := b.create(t, "Linux pool work")
	for _, test := range []struct {
		name       string
		fixture    nativeFixture
		runner     runnerFixture
		descriptor policy.Descriptor
		issue      tracker.NativeWorkItemID
		want       int
	}{
		{"Linux cannot take Mac work", a, linux, macPolicy, aIssue.WorkItemID, http.StatusConflict},
		{"Mac cannot take Linux work", b, mac, linuxPolicy, bIssue.WorkItemID, http.StatusConflict},
		{"Mac accepts exact work", a, mac, macPolicy, aIssue.WorkItemID, http.StatusOK},
		{"Linux accepts matching work", b, linux, linuxPolicy, bIssue.WorkItemID, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := tracker.NativeClaim{PolicyID: test.descriptor.ID, WorkItemID: test.issue, MachineID: test.runner.binding.MachineID, SessionID: test.name, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, test.fixture.base+"/claims", test.runner.redemption.Credential, claim), test.want)
		})
	}
}

func TestRunnerClaimRevalidatesStaleAuthentication(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, statement string }{
		{"credential revoked", "UPDATE api_tokens SET revoked_at = created_at WHERE id = $1"},
		{"credential rotated", "UPDATE api_tokens SET token_hash = '1111111111111111111111111111111111111111111111111111111111111111' WHERE id = $1"},
		{"access removed", "DELETE FROM token_grants WHERE token_id = $1"},
		{"disabled", "UPDATE runner_identities SET state = 'disabled' WHERE token_id = $1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "stale-authority")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
			r.enroll(t)
			f.create(t, "queued")
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			heldCredential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(r.redemption.Credential), "", "")
			if err != nil {
				t.Fatalf("held authentication status=%d error=%v", status, err)
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: heldCredential}
			if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), test.statement, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			_, err = f.service.claimNativeIssueTenant(t.Context(), scope, tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.binding.MachineID, SessionID: "stale", TTLSeconds: 60, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}, time.Minute)
			if err == nil {
				t.Fatal("stale middleware authentication authorized a new lease")
			}
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&count); err != nil || count != 0 {
				t.Fatalf("leases=%d error=%v", count, err)
			}
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type='scheduler.decision'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale authority mutated scheduling history: count=%d error=%v", count, err)
			}
		})
	}
}

func TestSpriteRunnerAccessRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, initial, report string
		tier                  *string
		sprite                bool
		status                int
	}{
		{"machine sandbox", "native-trusted", "", new("sandbox"), false, http.StatusOK},
		{"sprite sandbox requests a fresh probe", "native-trusted", `{"test":["native-trusted"]}`, new("sandbox"), true, http.StatusOK},
		{"sprite sandbox selection with unavailable backend", "native-trusted", `{"p/codex":["native-trusted","sandbox"],"p/claude":["native-trusted"]}`, new("sandbox"), true, http.StatusOK},
		{"sprite sandbox after passing probe", "native-trusted", `{"p/codex":["native-trusted","sandbox"]}`, new("sandbox"), true, http.StatusOK},
		{"legacy sprite preserved", "sandbox", "", nil, true, http.StatusOK},
		{"legacy sprite full access", "sandbox", "", new("native-trusted"), true, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.redemption.IsolationTier = test.initial
			if test.sprite {
				r.redemption.SpriteName = r.redemption.Hostname
			}
			r.enroll(t)
			if test.report != "" {
				var report isolation.Report
				if err := json.Unmarshal([]byte(test.report), &report); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerBackendIsolation(t.Context(), r.binding.RunnerID, report)
				}); err != nil {
					t.Fatal(err)
				}
			}
			var before runnerauth.Runner
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &before)
			request := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}, IsolationTier: test.tier}
			response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, request)
			requireNativeStatus(t, response, test.status)
			var after runnerauth.Runner
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &after)
			want, revision := test.initial, before.Revision
			if test.status == http.StatusOK {
				revision++
				if test.tier != nil {
					want = *test.tier
				}
			}
			if after.IsolationTier != want || after.Revision != revision {
				t.Fatalf("routing tier=%s revision=%d, want %s revision=%d", after.IsolationTier, after.Revision, want, revision)
			}
		})
	}
}
