package hubserver

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
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
				raw, err := marshalNative(states)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", raw, f.project.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET source_name=?, detent_state=? WHERE project_id=? AND detent_state='Todo'", test.lane, test.lane, f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
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
			runtimePath := f.base + "/work-items/" + string(issue.WorkItemID) + "/runtime"
			var evidence tracker.NativeRuntimeEvidence
			readDecision := func() {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &evidence)
			}
			readDecision()
			if test.want == http.StatusConflict && test.state != "draining" {
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
				var evaluatedLanding int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision' AND json_extract(data_json, '$.decision.source')='native_claim_eligibility'", issue.WorkItemID).Scan(&evaluatedLanding); err != nil || evaluatedLanding != 0 {
					t.Fatalf("ordinary claim manufactured Change readiness: count=%d error=%v", evaluatedLanding, err)
				}
				if test.name == "empty selector" {
					foreign := newNativeFixture(t, f.service, "", "foreign-routing")
					claim.WorkItemID = foreign.create(t, "foreign").WorkItemID
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/claims", r.redemption.Credential, claim), http.StatusNotFound)
					var count int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE project_id=? AND type='scheduler.decision'", foreign.project.ID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("foreign refusal mutated history: count=%d error=%v", count, err)
					}
				}
				return
			}
			if decision.Data.Decision.Outcome != "skipped" {
				t.Fatalf("refusal evidence = %#v", decision)
			}
			for range 3 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
			}
			readDecision()
			if evidence.LatestDecision.ID != decisionID {
				t.Fatal("unchanged refused evaluation appended duplicate history")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
			readDecision()
			if evidence.LatestDecision.ID == decisionID || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision+1 {
				t.Fatalf("changed revision lost fresh refusal: %#v", evidence.LatestDecision)
			}
		})
	}
}

func TestRunnerSettingsPersistAndReachHeartbeat(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "settings")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{
		DisplayName: "Build runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID},
		IsolationTier: "native-trusted", HostServices: []string{"tcp:127.0.0.1:8080"},
		Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, HardDeadline: "30m"},
	}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
	var stored runnerauth.Runner
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &stored)
	if stored.IsolationTier != "native-trusted" || stored.Availability.HardDeadline != "30m" || len(stored.HostServices) != 1 {
		t.Fatalf("stored settings = %#v", stored.Routing)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/projects/"+string(f.project.ID)+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"display_name": "Build runner", "capacity": 2, "version": "test", "os": "linux", "architecture": "arm64"})
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &snapshot)
	if snapshot.RunnerID != r.binding.RunnerID || snapshot.Revision != stored.Revision || snapshot.Routing.IsolationTier != "native-trusted" || snapshot.Routing.Availability.HardDeadline != "30m" {
		t.Fatalf("heartbeat routing = %#v", snapshot)
	}
	legacy := runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: runnerauth.Routing{DisplayName: "Renamed", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, legacy)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &stored)
	if stored.DisplayName != "Renamed" || stored.IsolationTier != "native-trusted" {
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
	decodeHubResponse(t, response, &stored)
	if stored.IsolationTier != "sandbox" || len(stored.Availability.Windows) != 0 || stored.Availability.HardDeadline != "" {
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
			before := f.service.config.now()
			heartbeatAt := before.Add(time.Second)
			observedAt := heartbeatAt.Add(test.elapsed)
			var clock atomic.Int64
			clock.Store(before.UnixNano())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			db := f.service.database.db
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			waits := db.Stats().WaitCount
			var result runnerauth.Runner
			var readErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, readErr = readRunnerWithClock(ctx, db, f.project.OrganizationID, r.binding.RunnerID, func() time.Time {
					return time.Unix(0, clock.Load()).UTC()
				})
			}()
			defer func() {
				cancel()
				_ = tx.Rollback()
				<-done
			}()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for db.Stats().WaitCount == waits {
				select {
				case <-done:
					t.Fatalf("runner read did not wait for the heartbeat transaction: %v", readErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", formatHubTime(heartbeatAt), r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			expiresAt := r.identity.ExpiresAt
			if test.expired {
				expiresAt = observedAt
			}
			var revokedAt any
			if test.revoked {
				revokedAt = formatHubTime(heartbeatAt)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE api_tokens SET expires_at = ?, revoked_at = ? WHERE id = ?", formatHubTime(expiresAt), revokedAt, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			clock.Store(observedAt.UnixNano())
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
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

func TestRunnerLeaseValidationAccounting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, statement, target, code string
		apiLimit                      int64
		fenceOffset                   tracker.FencingToken
		status                        int
	}{
		{name: "available with exhausted event window", apiLimit: 100, status: http.StatusOK},
		{name: "API window exhausted", apiLimit: 0, status: http.StatusTooManyRequests, code: "allowance_exhausted"},
		{name: "stale fence", apiLimit: 100, fenceOffset: 1, status: http.StatusConflict, code: "stale_fencing_token"},
		{name: "expired lease", apiLimit: 100, statement: "UPDATE leases SET expires_at=acquired_at WHERE lease_id=?", target: "lease", status: http.StatusConflict, code: "stale_fencing_token"},
		{name: "released lease", apiLimit: 100, statement: "UPDATE leases SET released_at=acquired_at WHERE lease_id=?", target: "lease", status: http.StatusConflict, code: "stale_fencing_token"},
		{name: "revoked credential", apiLimit: 100, statement: "UPDATE api_tokens SET revoked_at=created_at WHERE id=?", target: "runner", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "project access removed", apiLimit: 100, statement: "DELETE FROM token_grants WHERE token_id=?", target: "runner", status: http.StatusNotFound, code: "not_found"},
		{name: "policy approval removed", apiLimit: 100, statement: "DELETE FROM project_policies WHERE scope=?", target: "project", status: http.StatusConflict, code: "policy_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "lease-accounting")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "work", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			d := f.service.database
			seedHubMachine(t, f.service, "unrelated", now)
			if _, err := d.db.ExecContext(t.Context(), "UPDATE machines SET last_heartbeat_at='invalid',token_id=? WHERE id='unrelated'", bootstrapTokenID); err != nil {
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
				if _, err := d.db.ExecContext(t.Context(), test.statement, target); err != nil {
					t.Fatal(err)
				}
			}
			f.service.config.Hosted = &HostedConfig{}
			d.hostedOrganization = f.project.OrganizationID
			hostedTestPlans(t, f.service, map[string]int64{"api_mutations": test.apiLimit, "ingested_events": 0, "collaboration_bytes": 0, "history_records": 0})
			f.service.config.Hosted = nil
			metrics := hostedRunnerTransactionMetrics(nativeBase + "/leases/:lease/validate")
			before, err := d.hostedConsumption(t.Context(), d.db, now, metrics...)
			if err != nil {
				t.Fatal(err)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/validate", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken + test.fenceOffset})
			requireNativeStatus(t, response, test.status)
			if test.code != "" {
				var failure apiErrorResponse
				decodeHubResponse(t, response, &failure)
				if failure.Code != test.code {
					t.Fatalf("refusal=%s want=%s", failure.Code, test.code)
				}
			}
			after, err := d.hostedConsumption(t.Context(), d.db, now, metrics...)
			if err != nil {
				t.Fatal(err)
			}
			if test.status == http.StatusOK {
				before["api_mutations"]++
			}
			if !maps.Equal(before, after) {
				t.Fatalf("validation accounting=%v want=%v", after, before)
			}
			var expires string
			if err := d.db.QueryRowContext(t.Context(), "SELECT expires_at FROM leases WHERE lease_id=?", lease.ID).Scan(&expires); err != nil {
				t.Fatal(err)
			}
			if test.name != "expired lease" && expires != formatHubTime(lease.ExpiresAt) {
				t.Fatal("validation changed lease expiry")
			}
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
		{"nonowner allowlist edit", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{} }, false, http.StatusForbidden},
		{"unknown project", func(c *runnerauth.RoutingChange) { c.ProjectIDs = []tracker.ProjectID{"prj_unknown"} }, false, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "admin")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			if test.name == "nonowner allowlist edit" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_enrollments SET created_by = (SELECT id FROM api_tokens WHERE token_hash = ?) WHERE runner_id = ?", apikey.HashToken(f.token), r.binding.RunnerID); err != nil {
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
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type='scheduler.decision'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale authority mutated scheduling history: count=%d error=%v", count, err)
			}
		})
	}
}
