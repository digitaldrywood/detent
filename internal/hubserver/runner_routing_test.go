package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
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
	}{
		{"empty selector", nil, "active", policy.Requirements{}, http.StatusOK},
		{"all tags", []string{"linux", "build"}, "active", policy.Requirements{RequiredTags: []string{"build", "linux"}}, http.StatusOK},
		{"missing tag", []string{"linux"}, "active", policy.Requirements{RequiredTags: []string{"build", "linux"}}, http.StatusConflict},
		{"unknown tag", []string{"linux"}, "active", policy.Requirements{RequiredTags: []string{"unknown"}}, http.StatusConflict},
		{"wrong machine", []string{"linux"}, "active", policy.Requirements{MachineID: string(runnerauth.NewBinding().MachineID)}, http.StatusConflict},
		{"wrong runner", []string{"linux"}, "active", policy.Requirements{RunnerID: runnerauth.NewBinding().RunnerID}, http.StatusConflict},
		{"draining", nil, "draining", policy.Requirements{}, http.StatusConflict},
		{"disabled", nil, "disabled", policy.Requirements{}, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "routing")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			issue := f.create(t, "queued")
			settings := map[string]any{"expected_revision": 1, "display_name": "Build runner", "tags": test.tags, "state": test.state, "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, settings), http.StatusOK)
			descriptor := hubTestPolicy()
			descriptor.Requirements = test.requirements
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
			var leases int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil {
				t.Fatal(err)
			}
			wantLeases := 0
			if test.want == http.StatusOK {
				wantLeases = 1
			}
			if leases != wantLeases {
				t.Fatalf("leases = %d, want %d", leases, wantLeases)
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

func TestRunnerSharedHostConcurrentClaimsAndRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
	f := newNativeFixture(t, openTestService(t, cfg), "", "shared")
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	first.enroll(t)
	second := sharedRunner(t, first)
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
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
		if i%2 != 0 {
			r = second
		}
		issue := f.create(t, fmt.Sprintf("work-%d", i))
		claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: fmt.Sprintf("session-%d", i), TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
		workers.Go(func() {
			<-start
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
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
	for _, winner := range winners {
		other := first
		if winner.runner.binding.RunnerID == first.binding.RunnerID {
			other = second
		}
		path := f.base + "/leases/" + string(winner.lease.ID)
		for _, operation := range []string{"renew", "release", "validate"} {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+operation, other.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken, TTLSeconds: 300, Reason: "released"}), http.StatusNotFound)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", winner.runner.redemption.Credential, winner.claim), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/validate", winner.runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken}), http.StatusOK)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, cfg)
	response := performHubAPIRequest(t, f.service, http.MethodGet, first.base+"/runners", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var fleet []runnerauth.Runner
	decodeHubResponse(t, response, &fleet)
	if len(fleet) != 2 {
		t.Fatalf("fleet = %#v", fleet)
	}
	for _, r := range fleet {
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
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", winner.runner.redemption.Credential, winner.claim)
	requireNativeStatus(t, response, http.StatusConflict)
	var failure nativeError
	decodeHubResponse(t, response, &failure)
	if failure.Code != "runner_offline" {
		t.Fatalf("offline reason = %s", failure.Code)
	}
}

func TestRunnerRoutingRevocationAndDrain(t *testing.T) {
	t.Parallel()
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
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "revocation")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "First", State: "active", Tags: []string{"build"}, CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
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
				change.ProjectIDs = nil
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
			for _, action := range []string{"renew", "validate"} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/"+action, r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), test.want)
			}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "event"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: descriptor.ID, RunID: newNativeID("run"), AttemptID: newNativeID("attempt")}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", r.redemption.Credential, event), test.want)
		})
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
		{"unknown project", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{"prj_unknown"} }, false, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "admin")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
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

func TestSharedEnrollmentRequiresExplicitHostApproval(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "sharing")
	r := prepareRunner(t, f, runnerauth.Read)
	r.enroll(t)
	for _, test := range []struct {
		name         string
		shared       bool
		machine      tracker.MachineID
		organization string
		want         int
	}{
		{"implicit collision", false, r.binding.MachineID, string(f.project.OrganizationID), http.StatusConflict},
		{"unknown shared host", true, runnerauth.NewBinding().MachineID, string(f.project.OrganizationID), http.StatusConflict},
		{"approved shared host", true, r.binding.MachineID, string(f.project.OrganizationID), http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := runnerauth.NewBinding()
			binding.MachineID = test.machine
			request := runnerauth.EnrollmentRequest{Binding: binding, SharedMachine: test.shared, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+test.organization+"/runner-enrollments", testHubAdminToken, request), test.want)
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
		{"credential revoked", "UPDATE api_tokens SET revoked_at = created_at WHERE id = ?"},
		{"credential rotated", "UPDATE api_tokens SET token_hash = '1111111111111111111111111111111111111111111111111111111111111111' WHERE id = ?"},
		{"access removed", "DELETE FROM token_grants WHERE token_id = ?"},
		{"disabled", "UPDATE runner_identities SET state = 'disabled' WHERE token_id = ?"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "stale-authority")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
			r.enroll(t)
			f.create(t, "queued")
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: r.binding.RunnerID, Hash: apikey.HashToken(r.redemption.Credential), Scope: apiScopeWorker, NativeOnly: true, Runner: r.identity}}
			if _, err := f.service.database.db.ExecContext(t.Context(), test.statement, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			_, err := f.service.database.claimNext(t.Context(), tracker.ClaimRequest{MachineID: r.binding.MachineID, SessionID: "stale", TTL: time.Minute}, claimCandidateQuery{PolicyID: descriptor.ID, RequirePolicy: true, NativeScope: &scope}, time.Minute)
			if err == nil {
				t.Fatal("stale middleware authentication authorized a new lease")
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&count); err != nil || count != 0 {
				t.Fatalf("leases=%d error=%v", count, err)
			}
		})
	}
}
