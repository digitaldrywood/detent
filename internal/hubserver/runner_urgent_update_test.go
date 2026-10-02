package hubserver

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestUrgentRunnerUpdateFleet(t *testing.T) {
	f := newNativeFixture(t, nil, "", "urgent-update")
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/runner-update/urgent"
	now := f.service.config.now()
	heartbeat := func(r runnerFixture, version string, receipt *runnerauth.UpdateReceipt) runnerauth.RoutingSnapshot {
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
		heartbeat(r, test.version, nil)
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
		if m.older && wantState == "active" {
			wantState = "draining"
		}
		if err != nil || stored.State != wantState || (stored.UpdateRequest != nil) != m.older {
			t.Fatalf("version %s state=%s desired=%+v error=%v", m.version, stored.State, stored.UpdateRequest, err)
		}
		snapshot := heartbeat(m.runner, m.version, nil)
		if snapshot.Routing.State != wantState || (snapshot.Routing.UpdateRequest != nil) != m.older {
			t.Fatalf("heartbeat=%+v", snapshot)
		}
	}
	newIssue := f.create(t, "must stay queued")
	claim.WorkItemID = newIssue.WorkItemID
	claim.SessionID = "urgent-new"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", active.redemption.Credential, claim), http.StatusConflict)
	leasePath := f.base + "/leases/" + string(lease.ID)
	for _, operation := range []string{"renew", "validate"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, leasePath+"/"+operation, active.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 300}), http.StatusOK)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, leasePath+"/release", active.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	for _, m := range members {
		if !m.older {
			continue
		}
		applied := runnerauth.BuildEvidence{Version: "1.2.4", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}
		receipt := &runnerauth.UpdateReceipt{Request: *urgent.Request, Status: "running", Applied: &applied, Running: &applied, ObservedAt: now}
		snapshot := heartbeat(m.runner, "1.2.4", receipt)
		if snapshot.Routing.State != m.state {
			t.Fatalf("did not restore operator routing: %+v", snapshot)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", active.redemption.Credential, claim), http.StatusOK)
	late := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	late.enroll(t)
	if snapshot := heartbeat(late, "1.2.3", nil); snapshot.Routing.State != "draining" || snapshot.Routing.UpdateRequest == nil {
		t.Fatalf("late runner escaped urgent release: %+v", snapshot)
	}
	now = f.service.config.now()
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", formatHubTime(now.Add(-3*time.Minute)), late.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, late.binding.RunnerID, now)
	if err != nil || stored.State != "draining" || stored.UpdateRequest == nil || stored.Health != "offline" {
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
	snapshot := heartbeat(late, "1.2.3", oldReceipt)
	if snapshot.Routing.State != "draining" || snapshot.Routing.UpdateRequest.Version != "1.2.5" {
		t.Fatalf("superseded delivery interrupted its session: %+v", snapshot)
	}
}
