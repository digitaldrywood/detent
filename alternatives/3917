package hubserver

import (
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestUrgentRunnerUpdateHeartbeatSurvivesReplayExpiry(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"current"} {
		t.Run(format, func(t *testing.T) {
			start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			var clock atomic.Int64
			clock.Store(start.UnixNano())
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, deps: hubdeps.Deps{RetentionTimeout: frozenRetentionTimeout}})
			s := f.service

			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			if _, err := usagePostgresAdmin(t, s).ExecContext(t.Context(), `UPDATE api_tokens SET expires_at=$1 WHERE organization_id=$2 AND id=(SELECT token_id FROM runner_identities WHERE organization_id=$2 AND runner_id=$3)`, start.Add(48*time.Hour), f.project.OrganizationID, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if _, err := usagePostgresAdmin(t, s).ExecContext(t.Context(), `UPDATE runner_identities SET expires_at=$1 WHERE organization_id=$2 AND runner_id=$3`, start.Add(48*time.Hour), f.project.OrganizationID, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/runner-update/urgent"
			first := urgentRunnerUpdateChange{Version: "1.2.4", Confirm: true, IdempotencyKey: "first"}
			response := performHubAPIRequest(t, s, http.MethodPut, path, testHubAdminToken, first)
			requireNativeStatus(t, response, http.StatusAccepted)
			var urgent urgentRunnerUpdate
			decodeHubResponse(t, response, &urgent)
			requireCommandPayloadObject(t, s, f.project.OrganizationID, "urgent_runner_update", "first", response.Body.Bytes())

			requireNativeStatus(t, performHubAPIRequest(t, s, http.MethodPut, path, testHubAdminToken, urgentRunnerUpdateChange{ExpectedRevision: urgent.Revision, Version: "1.2.5", Confirm: true, IdempotencyKey: "second"}), http.StatusAccepted)
			clock.Store(start.Add(commandReceiptRetention + time.Second).UnixNano())
			ctx := s.contentContext(t.Context())
			if err := s.retainCommandPayloads(ctx, s.config.Clock()); err != nil {
				t.Fatal(err)
			}

			now := s.config.Clock()
			receipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: "refused", FailureReason: "Update was not applied", ObservedAt: now}
			build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			report := &runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: build, ObservedAt: now, Receipt: receipt}
			report.Revision = report.BuildRevision()
			forged := *receipt
			forged.Request.Version = "1.2.6"
			report.Receipt = &forged
			response = performHubAPIRequest(t, s, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": build.Version, "os": build.OS, "architecture": build.Architecture, "protocol_major": 2, "update": report, "backend_isolation": r.redemption.BackendIsolation})
			requireNativeStatus(t, response, http.StatusOK)
			routingCredential0, routingStatus0, routingError0 := s.authenticateTenantAPIHash(ctx, f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if routingError0 != nil {
				t.Fatalf("routing reader status=%d error=%v", routingStatus0, routingError0)
			}
			stored, err := s.readRunnerRouting(ctx, hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential0}, r.binding.RunnerID)
			if err != nil || stored.Update == nil || stored.Update.Receipt != nil {
				t.Fatalf("expired receipt authenticated a changed request: observation=%+v error=%v", stored.Update, err)
			}
			report.Receipt = receipt
			response = performHubAPIRequest(t, s, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": build.Version, "os": build.OS, "architecture": build.Architecture, "protocol_major": 2, "update": report, "backend_isolation": r.redemption.BackendIsolation})
			requireNativeStatus(t, response, http.StatusOK)
			routingCredential1, routingStatus1, routingError1 := s.authenticateTenantAPIHash(ctx, f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if routingError1 != nil {
				t.Fatalf("routing reader status=%d error=%v", routingStatus1, routingError1)
			}
			stored, err = s.readRunnerRouting(ctx, hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential1}, r.binding.RunnerID)
			if err != nil || stored.Update == nil || !reflect.DeepEqual(stored.Update.Receipt, receipt) {
				t.Fatalf("expired superseded urgent update lost runner receipt: observation=%+v error=%v", stored.Update, err)
			}
		})
	}
}

func TestUrgentRunnerUpdateFleet(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/runner-update/urgent"
	heartbeat := func(t *testing.T, f nativeFixture, r runnerFixture, version string, receipt *runnerauth.UpdateReceipt) runnerauth.RoutingSnapshot {
		t.Helper()
		build := runnerauth.BuildEvidence{Version: version, Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
		if receipt != nil && receipt.Status == "running" {
			build = *receipt.Applied
		}
		report := &runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: true, Running: build, ObservedAt: now, Receipt: receipt}
		report.Revision = report.BuildRevision()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": version, "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": report, "backend_isolation": r.redemption.BackendIsolation})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		return snapshot
	}
	type member struct {
		runner  runnerFixture
		version string
		state   string
		older   bool
	}
	members := []member{}
	for _, test := range []struct {
		version string
		state   string
		older   bool
	}{
		{"1.2.2", "active", true},
		{"v1.2.3", "active", true},
		{"1.2.4", "active", false},
		{"1.3.0", "active", false},
		{"1.2.3", "disabled", true},
		{"1.2.3", "draining", true},
	} {
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.redemption.BackendIsolation["codex"] = []string{isolation.NativeTrusted, isolation.Sandbox}
		r.enroll(t)
		r.reportCapacity(t)
		heartbeat(t, f, r, test.version, nil)
		if test.state != "active" {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "runner", "state": test.state, "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}}), http.StatusOK)
		}
		members = append(members, member{r, test.version, test.state, test.older})
	}
	active := members[0].runner
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	issue := f.create(t, "running through urgent drain")
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: active.binding.MachineID, SessionID: "urgent-live", TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", active.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	request := urgentRunnerUpdateChange{Version: "v1.2.4", Confirm: true, IdempotencyKey: "urgent-release"}
	response = performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusAccepted)
	var urgent urgentRunnerUpdate
	decodeHubResponse(t, response, &urgent)
	for _, m := range members {
		routingCredential2, routingStatus2, routingError2 := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
		if routingError2 != nil {
			t.Fatalf("routing reader status=%d error=%v", routingStatus2, routingError2)
		}
		stored, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential2}, m.runner.binding.RunnerID)
		wantState := m.state
		if err != nil || stored.State != wantState || (stored.UpdateRequest != nil) != m.older {
			t.Fatalf("version %s state=%s desired=%+v error=%v", m.version, stored.State, stored.UpdateRequest, err)
		}
		snapshot := heartbeat(t, f, m.runner, m.version, nil)
		if snapshot.Routing.State != wantState || (snapshot.Routing.UpdateRequest != nil) != m.older {
			t.Fatalf("heartbeat=%+v", snapshot)
		}
	}
	newIssue := f.create(t, "uses a free slot during routine update")
	claim.WorkItemID = newIssue.WorkItemID
	claim.SessionID = "urgent-new"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", active.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var concurrent tracker.NativeLease
	decodeHubResponse(t, response, &concurrent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(concurrent.ID)+"/release", active.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: concurrent.FencingToken, Reason: "completed"}), http.StatusNoContent)
	leasePath := f.base + "/leases/" + string(lease.ID)
	for _, operation := range []string{"renew", "validate"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, leasePath+"/"+operation, active.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 300}), http.StatusOK)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, leasePath+"/release", active.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	for _, m := range members {
		if !m.older {
			continue
		}
		for _, failure := range []struct{ status, reason string }{
			{"refused", "Update target resolution failed"},
			{"refused", "Update download failed"},
			{"refused", "Update signature verification failed"},
			{"refused", "Update provenance verification failed"},
			{"refused", "Update installation failed"},
			{"uncertain", "Update restart request was declined"},
			{"refused", ""},
		} {
			now = now.Add(time.Microsecond)
			receipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: failure.status, FailureReason: failure.reason, ObservedAt: now}
			snapshot := heartbeat(t, f, m.runner, m.version, receipt)
			if snapshot.Routing.State != m.state {
				t.Fatalf("failed update did not restore operator state: %+v", snapshot)
			}
			routingCredential3, routingStatus3, routingError3 := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if routingError3 != nil {
				t.Fatalf("routing reader status=%d error=%v", routingStatus3, routingError3)
			}
			stored, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential3}, m.runner.binding.RunnerID)
			if err != nil || stored.State != m.state || stored.Update.Receipt.FailureReason != failure.reason {
				t.Fatalf("runner lost failure: %+v %v", stored, err)
			}
			fleet := hostedFleetRunnerView(stored, m.version, map[tracker.ProjectID]bool{f.project.ID: true}, now)
			if fleet.Update.Observation.Receipt.FailureReason != failure.reason || fleet.State != m.state {
				t.Fatalf("fleet lost failure: %+v", fleet)
			}
			if fleet.Health != "needs_attention" || fleet.CapacityHealth != "online" {
				t.Fatalf("update failure changed capacity health: %+v", fleet)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, m.runner.identityPath()+"/update", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var view runnerauth.UpdateView
			decodeHubResponse(t, response, &view)
			if view.Observation.Receipt.FailureReason != failure.reason {
				t.Fatalf("update read lost failure: %+v", view)
			}
		}
		if m.runner.binding.RunnerID == active.binding.RunnerID {
			claim.SessionID = "urgent-retry"
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", active.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var resumed tracker.NativeLease
			decodeHubResponse(t, response, &resumed)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(resumed.ID)+"/release", active.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: resumed.FencingToken, Reason: "completed"}), http.StatusNoContent)
		}
		if snapshot := heartbeat(t, f, m.runner, m.version, &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: "applying", ObservedAt: now}); snapshot.Routing.State != m.state {
			t.Fatalf("update changed operator routing: %+v", snapshot)
		}
		applied := runnerauth.BuildEvidence{Version: "1.2.4", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}
		receipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: "running", Applied: &applied, Running: &applied, ObservedAt: now}
		snapshot := heartbeat(t, f, m.runner, "1.2.4", receipt)
		if snapshot.Routing.State != m.state {
			t.Fatalf("did not restore operator routing: %+v", snapshot)
		}
	}
	claim.SessionID = "urgent-completed"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", active.redemption.Credential, claim), http.StatusOK)
	late := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	late.enroll(t)
	if snapshot := heartbeat(t, f, late, "1.2.3", nil); snapshot.Routing.State != "active" || snapshot.Routing.UpdateRequest == nil {
		t.Fatalf("late runner escaped urgent release: %+v", snapshot)
	}
	now = f.service.config.Clock()
	if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2 AND runner_id=$3", now.Add(-3*time.Minute), f.project.OrganizationID, late.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	routingCredential4, routingStatus4, routingError4 := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if routingError4 != nil {
		t.Fatalf("routing reader status=%d error=%v", routingStatus4, routingError4)
	}
	stored, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential4}, late.binding.RunnerID)
	if err != nil || stored.State != "active" || stored.UpdateRequest == nil || stored.Health != "offline" {
		t.Fatalf("offline runner escaped urgent release: %+v error=%v", stored, err)
	}
	for _, version := range []string{"1.2.4", "1.2.5"} {
		request = urgentRunnerUpdateChange{ExpectedRevision: urgent.Revision, Version: version, Confirm: true, IdempotencyKey: "urgent-" + version}
		response = performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, request)
		requireNativeStatus(t, response, http.StatusAccepted)
		var next urgentRunnerUpdate
		decodeHubResponse(t, response, &next)
		if version == "1.2.4" && *next.Request != *urgent.Request {
			t.Fatal("remarking the same release replaced its delivery")
		}
		urgent.Revision = next.Revision
	}
	oldReceipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: "draining", ObservedAt: now}
	snapshot := heartbeat(t, f, late, "1.2.3", oldReceipt)
	if snapshot.Routing.State != "active" || snapshot.Routing.UpdateRequest.Version != "1.2.5" {
		t.Fatalf("superseded delivery interrupted its session: %+v", snapshot)
	}
	foreign := *urgent.Request
	foreign.ID = "update_superseded_elsewhere"
	if snapshot := heartbeat(t, f, late, "1.2.3", &runnerauth.UpdateReceipt{Request: foreign, Status: "draining", ObservedAt: now}); snapshot.Routing.UpdateRequest == nil || snapshot.Routing.UpdateRequest.Version != "1.2.5" {
		t.Fatalf("an unrecognized update receipt blocked the heartbeat: %+v", snapshot)
	}
	const runningVersion = "0.121.4-op.r.325.gaaaaaaaaaaaa"
	const targetVersion = "0.121.4-op.r.326.gbbbbbbbbbbbb"
	for _, test := range []struct {
		name, status, state     string
		superseded, newerUrgent bool
	}{
		{name: "satisfied refused urgent", status: "refused", state: "active"},
		{name: "completed urgent", status: "running", state: "active"},
		{name: "satisfied uncertain urgent", status: "uncertain", state: "active"},
		{name: "operator drain", status: "refused", state: "draining"},
		{name: "operator disabled", status: "refused", state: "disabled"},
		{name: "superseded refused urgent", status: "refused", state: "active", superseded: true},
		{name: "superseded uncertain urgent", status: "uncertain", state: "active", superseded: true},
		{name: "newer urgent takes precedence", status: "refused", state: "active", newerUrgent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/runner-update/urgent"
			response := performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, urgentRunnerUpdateChange{Version: runningVersion, Confirm: true, IdempotencyKey: "urgent-325"})
			requireNativeStatus(t, response, http.StatusAccepted)
			var urgent urgentRunnerUpdate
			decodeHubResponse(t, response, &urgent)
			f.service.config.Version = "v" + targetVersion
			f.service.runnerPublishedReleases.Store("v"+targetVersion+"/linux/amd64", true)
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.redemption.BackendIsolation["codex"] = []string{isolation.NativeTrusted, isolation.Sandbox}
			r.enroll(t)
			r.reportCapacity(t)
			if test.state != "active" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "runner", "state": test.state, "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}}), http.StatusOK)
			}
			applied := runnerauth.BuildEvidence{Version: runningVersion, Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}
			receipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: test.status, ObservedAt: now}
			if test.status == "running" {
				receipt.Applied, receipt.Running = &applied, &applied
			} else {
				receipt.FailureReason = "Update was not applied"
			}
			reportedVersion, deliveredVersion := runningVersion, targetVersion
			if test.superseded || test.newerUrgent {
				initial := heartbeat(t, f, r, runningVersion, nil)
				if initial.Routing.UpdateRequest == nil || !initial.Routing.UpdateRequest.FollowHub || initial.Routing.UpdateRequest.Version != targetVersion {
					t.Fatalf("did not persist initial follow-Hub delivery: %+v", initial.Routing.UpdateRequest)
				}
			}
			if test.superseded {
				reportedVersion = "0.121.4-op.r.324.gcccccccccccc"
			}
			if test.newerUrgent {
				deliveredVersion = "0.121.4-op.r.327.gdddddddddddd"
				response = performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, urgentRunnerUpdateChange{ExpectedRevision: urgent.Revision, Version: deliveredVersion, Confirm: true, IdempotencyKey: "urgent-327"})
				requireNativeStatus(t, response, http.StatusAccepted)
			}
			snapshot := heartbeat(t, f, r, reportedVersion, receipt)
			request := snapshot.Routing.UpdateRequest
			if request == nil || request.Version != deliveredVersion || request.FollowHub == test.newerUrgent || request.Urgent != test.newerUrgent || request.Validate() != nil {
				t.Fatalf("wrong delivered request: got=%+v want_version=%s state=%s", request, deliveredVersion, snapshot.Routing.State)
			}
			wantState := test.state
			if snapshot.Routing.State != wantState {
				t.Fatalf("pending update lost effective drain: %+v", snapshot)
			}
			repeated := heartbeat(t, f, r, reportedVersion, receipt)
			if repeated.Revision != snapshot.Revision || repeated.Routing.UpdateRequest == nil || *repeated.Routing.UpdateRequest != *request {
				t.Fatalf("heartbeat replaced newer delivery: %+v", repeated)
			}
			routingCredential5, routingStatus5, routingError5 := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if routingError5 != nil {
				t.Fatalf("routing reader status=%d error=%v", routingStatus5, routingError5)
			}
			stored, err := f.service.readRunnerRouting(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: routingCredential5}, r.binding.RunnerID)
			if err != nil || stored.UpdateRequest == nil || *stored.UpdateRequest != *request || stored.Update == nil || !reflect.DeepEqual(stored.Update.Receipt, receipt) {
				t.Fatalf("routing lost request or historical receipt: %+v error=%v", stored, err)
			}
			fleet := hostedFleetRunnerView(stored, reportedVersion, map[tracker.ProjectID]bool{f.project.ID: true}, now)
			if fleet.State != wantState || fleet.Update.Desired == nil || *fleet.Update.Desired != *request || fleet.Update.Observation.Receipt.Request != receipt.Request {
				t.Fatalf("fleet lost effective update routing: %+v", fleet)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/update", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var view runnerauth.UpdateView
			decodeHubResponse(t, response, &view)
			if view.Desired == nil || *view.Desired != *request || view.Observation.Receipt.Request != receipt.Request || view.Observation.Receipt.Status != receipt.Status {
				t.Fatalf("update read lost retained receipt: %+v", view)
			}
			queued := f.create(t, "queued during follow-Hub update")
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(queued.WorkItemID)+"/runtime", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, response, &evidence)
			found := false
			for _, admission := range evidence.Admission {
				if admission.RunnerID != r.binding.RunnerID {
					continue
				}
				found = true
				wantCode := "runner_draining"
				if test.state == "disabled" {
					wantCode = "runner_disabled"
				}
				if test.state == "active" {
					if admission.Outcome == "skipped" && admission.ReasonCode == "runner_draining" {
						t.Fatalf("routine update withheld a free slot: %+v", admission)
					}
				} else if admission.Outcome != "skipped" || admission.ReasonCode != wantCode {
					t.Fatalf("admission ignored effective runner state with free slots: %+v", admission)
				}
			}
			if !found {
				t.Fatal("admission omitted the updating runner")
			}
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: queued.WorkItemID, MachineID: r.binding.MachineID, SessionID: "follow-hub", TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			claimResponse := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			switch test.state {
			case "active":
				requireNativeStatus(t, claimResponse, http.StatusOK)
				var admitted tracker.NativeLease
				decodeHubResponse(t, claimResponse, &admitted)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(admitted.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: admitted.FencingToken, Reason: "completed"}), http.StatusNoContent)
			case "draining":
				requireNativeError(t, claimResponse, http.StatusConflict, "runner_draining")
			default:
				requireNativeError(t, claimResponse, http.StatusConflict, "runner_disabled")
			}
			applied.Version = deliveredVersion
			applied.Commit = strings.Repeat("c", 40)
			applied.SHA256 = strings.Repeat("d", 64)
			receipt = &runnerauth.UpdateReceipt{Request: *request, Status: "draining", ObservedAt: now}
			if snapshot := heartbeat(t, f, r, reportedVersion, receipt); snapshot.Routing.State != wantState {
				t.Fatalf("update receipt lost graceful drain: %+v", snapshot)
			}
			receipt.Status, receipt.Applied, receipt.Running = "running", &applied, &applied
			if snapshot := heartbeat(t, f, r, deliveredVersion, receipt); snapshot.Routing.State != test.state {
				t.Fatalf("completed update lost operator state: %+v", snapshot)
			}
			if test.state == "active" {
				claim.SessionID = "follow-hub-after-update"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusOK)
			}
		})
	}
}
