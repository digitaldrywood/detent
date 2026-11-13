package hubserver

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerIsolationClaims(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		report       isolation.Report
		tier         string
		want         int
		requiredTags []string
		stalePolicy  bool
	}{
		{"missing report", nil, "sandbox", http.StatusConflict, nil, false},
		{"sandbox", isolation.Report{"codex": {"sandbox", "native-trusted"}}, "sandbox", http.StatusOK, nil, false},
		{"native cannot sandbox", isolation.Report{"codex": {"native-trusted"}}, "sandbox", http.StatusConflict, nil, false},
		{"trusted", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusOK, nil, false},
		{"trusted missing report", nil, "native-trusted", http.StatusConflict, nil, false},
		{"later heartbeat restores sandbox", isolation.Report{"codex": {"native-trusted"}}, "sandbox", http.StatusConflict, nil, false},
		{"sandbox project selector excludes trusted runner", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusConflict, []string{"sandbox"}, false},
		{"mixed backends", isolation.Report{"codex": {"sandbox"}, "claude": {"native-trusted"}}, "sandbox", http.StatusConflict, nil, false},
		{"probe failed", isolation.Report{"codex": {}}, "sandbox", http.StatusConflict, nil, false},
		{"stale permissive policy cannot bypass current selector", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusConflict, []string{"sandbox"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newPostgresNativeService(t, Config{})
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					f := newNativeFixture(t, service, organization, "Example project")
					r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					r.redemption.BackendIsolation = test.report
					r.enroll(t)
					r.reportCapacity(t)
					issue := f.create(t, "queued")
					settings := map[string]any{"expected_revision": 1, "display_name": "Runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}, "isolation_tier": test.tier}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, settings), http.StatusOK)
					permissive := hubTestPolicy()
					if test.stalePolicy {
						approveHubTestPolicy(t, f.service, f.base+"/policy", permissive)
					}
					descriptor := hubTestPolicy()
					descriptor.Requirements = policy.Requirements{RequiredTags: test.requiredTags}
					descriptor.ID = ""
					descriptor = descriptor.WithID()
					approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
					claimPolicy := descriptor.ID
					if test.stalePolicy {
						claimPolicy = permissive.ID
					}
					claim := tracker.NativeClaim{PolicyID: claimPolicy, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
					if test.name == "later heartbeat restores sandbox" {
						r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
						r.reportCapacity(t)
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusOK)
					}
				})
			}
		})
	}
}

func TestRunnerIsolationHeartbeatWithdrawsTier(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		previousSandbox bool
		sandbox         bool
	}{
		{"previously proven sandbox", true, true},
		{"first sandbox probe", false, true},
		{"failed sandbox probe", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.redemption.IsolationTier = isolation.NativeTrusted
			r.redemption.SpriteName = r.redemption.Hostname
			r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
			if test.previousSandbox {
				r.redemption.BackendIsolation["codex"] = append(r.redemption.BackendIsolation["codex"], isolation.Sandbox)
			}
			r.enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"codex": {isolation.NativeTrusted}},
				"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.service.config.Clock()}}
			postHeartbeat := func() {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, heartbeat), http.StatusOK)
			}
			postHeartbeat()
			var before runnerauth.Runner
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &before)
			if isolation.Report(before.BackendIsolation).Supports(isolation.Sandbox) || !isolation.Report(before.BackendIsolation).Supports(isolation.NativeTrusted) {
				t.Fatalf("native heartbeat retained sandbox proof: %+v", before.BackendIsolation)
			}
			request := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}, IsolationTier: new(isolation.Sandbox)}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, request), http.StatusOK)
			issue := f.create(t, "queued")
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "sandbox-transition", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
			want := http.StatusConflict
			if test.sandbox {
				heartbeat["backend_isolation"] = isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}}
				want = http.StatusOK
			} else {
				problem := runnerauth.NewProblem("tier_unavailable")
				problem.Subject = "codex/sandbox"
				heartbeat["problems"] = []runnerauth.Problem{problem}
			}
			postHeartbeat()
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, want)
			if test.sandbox {
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.IsolationPolicy == nil || lease.IsolationPolicy.Tier != isolation.Sandbox {
					t.Fatalf("sandbox claim policy = %+v", lease.IsolationPolicy)
				}
			}
		})
	}
}

func TestRunnerIsolationOmittedHeartbeatClearsReport(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"display_name": "Runner", "capacity": 2, "version": "test"}), http.StatusOK)
	var report isolation.Report
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		report, err = reader.RunnerBackendIsolation(t.Context(), r.binding.RunnerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(report) != 0 {
		t.Fatalf("stale report retained: %v", report)
	}
}

func TestRunnerIsolationPolicyPinnedToClaim(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	r.reportCapacity(t)
	issue := f.create(t, "queued")
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "pinned", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.IsolationPolicy == nil || lease.IsolationPolicy.Tier != isolation.Sandbox {
		t.Fatalf("claim isolation = %#v", lease.IsolationPolicy)
	}
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, IsolationTier: isolation.NativeTrusted}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
	for _, request := range []struct {
		path string
		body any
	}{
		{f.base + "/claims", claim},
		{f.base + "/leases/" + string(lease.ID) + "/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}},
	} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, request.path, r.redemption.Credential, request.body)
		requireNativeStatus(t, response, http.StatusOK)
		var renewed tracker.NativeLease
		decodeHubResponse(t, response, &renewed)
		if renewed.IsolationPolicy == nil || renewed.IsolationPolicy.Tier != isolation.Sandbox {
			t.Fatalf("claim downgraded: %#v", renewed.IsolationPolicy)
		}
	}
}

func TestRunnerIsolationLeaseLifecycle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, initial, changed string
		legacy                 bool
	}{
		{"sandbox remains sandbox", isolation.Sandbox, isolation.NativeTrusted, false},
		{"trusted remains trusted", isolation.NativeTrusted, isolation.Sandbox, false},
		{"legacy follows trusted", isolation.Sandbox, isolation.NativeTrusted, true},
		{"legacy follows sandbox", isolation.NativeTrusted, isolation.Sandbox, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newPostgresNativeService(t, Config{})
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					f := newNativeFixture(t, service, organization, "Example project")
					r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
					r.redemption.IsolationTier = test.initial
					r.enroll(t)
					r.reportCapacity(t)
					issue := f.create(t, "queued")
					descriptor := hubTestPolicy()
					approveHubTestPolicy(t, service, f.base+"/policy", descriptor)
					claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "pinned", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
					response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
					requireNativeStatus(t, response, http.StatusOK)
					var lease tracker.NativeLease
					decodeHubResponse(t, response, &lease)
					if lease.IsolationPolicy == nil || lease.IsolationPolicy.Tier != test.initial {
						t.Fatalf("initial claim isolation = %+v", lease.IsolationPolicy)
					}
					if test.legacy {
						if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), `UPDATE leases SET isolation_tier=NULL WHERE organization_id=$1 AND lease_id=$2`, organization, lease.ID); err != nil {
							t.Fatal(err)
						}
					}
					change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, IsolationTier: test.changed}}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
					want := test.initial
					if test.legacy {
						want = test.changed
					}
					for _, request := range []struct {
						path string
						body any
					}{
						{f.base + "/claims", claim},
						{f.base + "/leases/" + string(lease.ID) + "/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}},
					} {
						response := performHubAPIRequest(t, service, http.MethodPost, request.path, r.redemption.Credential, request.body)
						requireNativeStatus(t, response, http.StatusOK)
						var renewed tracker.NativeLease
						decodeHubResponse(t, response, &renewed)
						if renewed.ID != lease.ID || renewed.IsolationPolicy == nil || renewed.IsolationPolicy.Tier != want {
							t.Fatalf("replayed or renewed isolation = %+v, want %s", renewed, want)
						}
					}
					start := nativeStartedEvent(lease)
					finish := start
					finish.Type, finish.IdempotencyKey = "run.finished", "finish"
					finish.Data.Sequence, finish.Data.Outcome = 2, "succeeded"
					finish.Data.CompletionBody = "Completed attempt details"
					path := f.base + "/work-items/" + string(issue.WorkItemID)
					for _, event := range []tracker.NativeRunEvent{start, finish} {
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", r.redemption.Credential, event), http.StatusOK)
						if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
							stored, err := reader.Lease(t.Context(), string(lease.ID))
							if err != nil {
								return err
							}
							if test.legacy && stored.IsolationTier != nil || !test.legacy && (stored.IsolationTier == nil || *stored.IsolationTier != test.initial) {
								t.Fatalf("%s changed stored isolation: %+v", event.Type, stored.IsolationTier)
							}
							details, err := service.config.NativeRunEvents.NativeLeaseResponse(t.Context(), reader, hubscope.NativeScope{Organization: organization, Project: f.project.ID}, stored)
							if err == nil && (details.IsolationPolicy == nil || details.IsolationPolicy.Tier != want) {
								t.Fatalf("%s isolation = %+v, want %s", event.Type, details.IsolationPolicy, want)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"/attempts", r.redemption.Credential, nil), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
					claim.SessionID = "after-change"
					response = performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
					requireNativeStatus(t, response, http.StatusOK)
					var next tracker.NativeLease
					decodeHubResponse(t, response, &next)
					if next.ID == lease.ID || next.IsolationPolicy == nil || next.IsolationPolicy.Tier != test.changed {
						t.Fatalf("new claim isolation = %+v, want %s", next, test.changed)
					}
					if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
						previous, err := reader.Lease(t.Context(), string(lease.ID))
						if err != nil {
							return err
						}
						if previous.ReleasedAt == nil || test.legacy && previous.IsolationTier != nil || !test.legacy && (previous.IsolationTier == nil || *previous.IsolationTier != test.initial) {
							t.Fatalf("released lease lost isolation: %+v", previous)
						}
						current, err := reader.Lease(t.Context(), string(next.ID))
						if err == nil && (current.IsolationTier == nil || *current.IsolationTier != test.changed) {
							t.Fatalf("new lease did not pin changed tier: %+v", current)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestRunnerIsolationRefusalAfterHeartbeatDuringClaim(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	f := newDefaultNativeFixture(t, Config{Clock: clock.Now})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	r.reportCapacity(t)
	issue := f.create(t, "queued")
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(r.redemption.Credential), "", "")
	if err != nil {
		t.Fatalf("runner authentication status=%d: %v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "heartbeat-during-claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	claimStarted := clock.Now()
	plan, err := f.service.readNativeClaimPlan(t.Context(), scope, claim, claimStarted)
	if err != nil {
		t.Fatalf("read claim plan: %v", err)
	}
	clock.Advance(time.Second)
	r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
	r.reportCapacity(t)
	if _, err := f.service.claimNativeIssueTenantWithPlan(t.Context(), scope, claim, time.Minute, claimStarted, plan, nil); !errors.Is(err, ErrNoClaimableWork) {
		t.Fatalf("claim after a heartbeat withdrew sandbox = %v, want %v", err, ErrNoClaimableWork)
	}
}
