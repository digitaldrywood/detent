package hubserver

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestUrgentRunnerUpdateFleet(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
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
		r.enroll(t)
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
	for _, test := range []struct {
		name       string
		credential string
		path       string
		change     func(*urgentRunnerUpdateChange)
		want       int
	}{
		{"confirmation", testHubAdminToken, path, func(r *urgentRunnerUpdateChange) { r.Confirm = false }, http.StatusPreconditionRequired},
		{"worker", active.redemption.Credential, path, func(*urgentRunnerUpdateChange) {}, http.StatusForbidden},
		{"other organization", testHubAdminToken, "/api/v2/organizations/org_other/runner-update/urgent", func(*urgentRunnerUpdateChange) {}, http.StatusNotFound},
		{"invalid release", testHubAdminToken, path, func(r *urgentRunnerUpdateChange) { r.Version = "dev" }, http.StatusUnprocessableEntity},
		{"stale revision", testHubAdminToken, path, func(r *urgentRunnerUpdateChange) { r.ExpectedRevision = 3 }, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := request
			test.change(&copy)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, test.path, test.credential, copy), test.want)
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusAccepted)
	var urgent urgentRunnerUpdate
	decodeHubResponse(t, response, &urgent)
	if urgent.Revision != 1 || urgent.Request == nil || !urgent.Request.Urgent || urgent.Request.Validate() != nil {
		t.Fatalf("urgent release=%+v", urgent)
	}
	replay := performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusAccepted)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("urgent replay changed")
	}
	request.Version = "1.2.5"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, request), http.StatusConflict)
	for _, m := range members {
		stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, m.runner.binding.RunnerID, now)
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
			receipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: failure.status, FailureReason: failure.reason, ObservedAt: now}
			snapshot := heartbeat(t, f, m.runner, m.version, receipt)
			if snapshot.Routing.State != m.state {
				t.Fatalf("failed update did not restore operator state: %+v", snapshot)
			}
			stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, m.runner.binding.RunnerID, now)
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
	now = f.service.config.now()
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", formatHubTime(now.Add(-3*time.Minute)), late.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, late.binding.RunnerID, now)
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
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
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
			r.enroll(t)
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
			stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, now)
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
			if test.state == "active" {
				requireNativeStatus(t, claimResponse, http.StatusOK)
				var admitted tracker.NativeLease
				decodeHubResponse(t, claimResponse, &admitted)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(admitted.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: admitted.FencingToken, Reason: "completed"}), http.StatusNoContent)
			} else {
				requireNativeStatus(t, claimResponse, http.StatusConflict)
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusOK)
			}
		})
	}
}

func TestApplyUrgentRunnerRoutingFollowHubReceipt(t *testing.T) {
	request := runnerauth.UpdateRequest{ID: "follow-0.129.3", Service: "detent", RequestedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Version: "0.129.3", Release: true, FollowHub: true}
	earlier := request
	earlier.ID = "follow-0.129.3-earlier"
	tests := []struct {
		name    string
		receipt *runnerauth.UpdateReceipt
		want    string
	}{
		{name: "no receipt stays active", want: "active"},
		{name: "refused receipt stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "refused", FailureReason: "update is unavailable for unknown installs"}, want: "active"},
		{name: "uncertain receipt with failure stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "uncertain", FailureReason: "download failed"}, want: "active"},
		{name: "uncertain receipt without failure stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "uncertain"}, want: "active"},
		{name: "applied receipt stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "applied"}, want: "active"},
		{name: "refused receipt for an earlier request stays active", receipt: &runnerauth.UpdateReceipt{Request: earlier, Status: "refused", FailureReason: "update is unavailable for unknown installs"}, want: "active"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := runnerauth.Runner{Routing: runnerauth.Routing{State: "active", UpdateRequest: &request}, Update: &runnerauth.UpdateObservation{Running: runnerauth.BuildEvidence{Version: "0.129.2"}, Receipt: test.receipt}}
			applyUrgentRunnerRouting(&r, urgentRunnerUpdate{}, "0.129.2")
			if r.State != test.want {
				t.Fatalf("state = %q, want %q", r.State, test.want)
			}
		})
	}
}
