package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerUpdateApplication(t *testing.T) {
	f := newNativeFixture(t, nil, "", "runner-update")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	now := time.Now().UTC()
	build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "private_patched_source", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
	report := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "1.2.4", AvailableObservedAt: now, Running: build, ObservedAt: now}
	report.Revision = report.BuildRevision()
	send := func(evidence *runnerauth.UpdateObservation, protocol int) runnerauth.RoutingSnapshot {
		version := "1.2.3"
		if evidence != nil {
			version = evidence.Running.Version
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": version, "os": "linux", "architecture": "amd64", "protocol_major": protocol, "update": evidence, "backend_isolation": r.redemption.BackendIsolation})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		return snapshot
	}
	snapshot := send(report, 2)
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	issue := f.create(t, "active update work")
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "active-update", TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	leaseResponse := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, leaseResponse, http.StatusOK)
	var active tracker.NativeLease
	decodeHubResponse(t, leaseResponse, &active)

	read := func() runnerauth.UpdateView {
		var view runnerauth.UpdateView
		response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/update", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &view)
		return view
	}
	view := read()
	if view.Status != "ready" || view.Observation.Running.Source != "private_patched_source" || view.Observation.Running.VerifiedRelease {
		t.Fatalf("initial view=%+v", view)
	}
	request := runnerUpdateChange{ExpectedRevision: snapshot.Revision, ExpectedBuildRevision: report.Revision, Service: "detent", Version: "1.2.4", Release: true, Confirm: true, IdempotencyKey: "selected-release"}
	staleBuild := request
	staleBuild.ExpectedBuildRevision = strings.Repeat("e", 64)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, staleBuild), http.StatusConflict)
	unavailable := *report
	unavailable.Supported = false
	unavailable.Revision = unavailable.BuildRevision()
	send(&unavailable, 2)
	if got := read(); got.Status != "unavailable" || got.Observation == nil || got.Observation.Protocol != 1 {
		t.Fatalf("missing installed owner=%+v", got)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request), http.StatusServiceUnavailable)
	send(report, 2)
	noConfirm := request
	noConfirm.Confirm = false
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, noConfirm), http.StatusPreconditionRequired)
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusAccepted)
	replay := performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusAccepted)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("changed idempotent receipt")
	}
	view = read()
	if view.Status != "requested" || view.Desired == nil || view.Observation.Receipt != nil {
		t.Fatalf("queued receipt=%+v", view)
	}
	for _, test := range []struct {
		name       string
		change     func(*runnerUpdateChange)
		credential string
		path       string
		want       int
	}{
		{"changed retry", func(c *runnerUpdateChange) { c.Version = "1.2.5" }, testHubAdminToken, r.identityPath() + "/update/apply", http.StatusConflict},
		{"stale request", func(c *runnerUpdateChange) { c.IdempotencyKey = "stale" }, testHubAdminToken, r.identityPath() + "/update/apply", http.StatusConflict},
		{"worker denied", func(c *runnerUpdateChange) {}, r.redemption.Credential, r.identityPath() + "/update/apply", http.StatusForbidden},
		{"cross organization", func(c *runnerUpdateChange) {}, testHubAdminToken, "/api/v2/organizations/org_other/runners/" + r.binding.RunnerID + "/update/apply", http.StatusNotFound},
		{"shared hub service denied", func(c *runnerUpdateChange) { c.Service = "hub" }, testHubAdminToken, r.identityPath() + "/update/apply", http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := request
			test.change(&copy)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, test.path, test.credential, copy), test.want)
		})
	}
	snapshot = send(report, 2)
	if snapshot.Routing.UpdateRequest == nil || *snapshot.Routing.UpdateRequest != *view.Desired || snapshot.Routing.CapacityLimit != 2 || snapshot.Routing.State != "active" {
		t.Fatalf("delivery=%+v", snapshot)
	}
	applied := build
	applied.Version = "1.2.4"
	applied.Commit = strings.Repeat("c", 40)
	applied.SHA256 = strings.Repeat("d", 64)
	applied.Source = "release"
	applied.VerifiedRelease = true
	report.Receipt = &runnerauth.UpdateReceipt{Request: *view.Desired, Status: "applied", Applied: &applied, ObservedAt: now}
	send(report, 2)
	if got := read(); got.Status != "applied" || got.Observation.Running.Version == got.Desired.Version {
		t.Fatalf("premature running receipt=%+v", got)
	}
	report.Running = applied
	report.Receipt.Status = "running"
	report.Revision = report.BuildRevision()
	send(report, 2)
	if got := read(); got.Status != "running" || !got.Observation.Running.VerifiedRelease {
		t.Fatalf("running receipt=%+v", got)
	}
	report.Receipt.Request.ID = "foreign"
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "1.2.4", "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": json.RawMessage(raw)}), http.StatusUnprocessableEntity)
	report.Receipt.Request.ID = view.Desired.ID
	report.Running.Source = "private_patched_source"
	report.Running.VerifiedRelease = false
	report.Revision = report.BuildRevision()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "1.2.4", "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": report}), http.StatusUnprocessableEntity)
	send(nil, 1)
	if got := read(); got.Status != "unavailable" || got.Observation != nil {
		t.Fatalf("older runner owner=%+v", got)
	}
	request.IdempotencyKey = "absent-owner"
	request.ExpectedRevision = view.Revision
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request), http.StatusServiceUnavailable)
	stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
	if err != nil || len(stored.Leases) != 1 || stored.Leases[0].ID != active.ID {
		t.Fatalf("active lease changed: %+v error=%v", stored.Leases, err)
	}
	var events int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE operation=?", "runner_update "+r.binding.RunnerID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit count=%d error=%v", events, err)
	}
}
